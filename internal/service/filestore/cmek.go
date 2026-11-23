package filestore

import (
	"context"
	"crypto/rand"
	"fmt"

	"cloud.google.com/go/filestore/apiv1/filestorepb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Customer-managed encryption keys (#2192), as
// https://docs.cloud.google.com/filestore/docs/cmek documents them (read
// 2026-11-23):
//
//   - Zonal, Regional and Enterprise instances take one (kms_key_name);
//     Basic HDD and Basic SSD do not, nor do basic tier backups;
//   - "the key ring and instance must be located in the same region"
//     (a backup's key, its backup's region); the key may be in another
//     project;
//   - the Filestore service agent,
//     service-PROJECT_NUMBER@cloud-filer.iam.gserviceaccount.com, needs
//     roles/cloudkms.cryptoKeyEncrypterDecrypter on it;
//   - when the key is disabled or destroyed, or the agent loses it, the
//     instance "automatically stops serving data" (state SUSPENDED,
//     suspension reason KMS_KEY_ISSUE: "The KMS key used by the instance
//     is either revoked or denied access to"); once the key can be used
//     again it restarts on its own.
//
// Here the instance's NFS server then serves no export, so its clients'
// handles go stale, and serves its share again once the key can be used:
// the keys are checked every KeyCheckEvery. A backup with a key is
// encrypted: its data is sealed (AES-256-GCM) with a data key that only
// Cloud KMS can unwrap, so a restore needs the key usable.

// AgentDomain is the Filestore service agent's domain.
const AgentDomain = "cloud-filer.iam.gserviceaccount.com"

// agentOf is project's Filestore service agent.
func agentOf(project string) string {
	return fmt.Sprintf("service-%d@%s", resource.ProjectNumber(project), AgentDomain)
}

// keyError is a key refusal as the API answers it.
func keyError(field string, err error) error {
	if e := cmek.As(err); e != nil {
		// UNVERIFIED wording: the CMEK page quotes no error (re-read
		// 2026-11-23: https://docs.cloud.google.com/filestore/docs/cmek).
		out := apierror.InvalidArgument("%s %s cannot be used: %s", field, e.Key, e.Msg)
		out.Code = e.Code()
		return out
	}
	return err
}

// checkKey checks a new resource's key: its name and region, and that the
// project's Filestore agent may use it.
func (s *Server) checkKey(ctx context.Context, field, key, project, location string) error {
	if s.opts.Keys == nil {
		return apierror.Unimplemented("%s needs Cloud KMS, which this instance does not serve: start it with --services kms", field)
	}
	if err := cmek.CheckName(field, key, regionOf(location)); err != nil {
		return keyError(field, err)
	}
	return keyError(field, s.opts.Keys.Usable(ctx, project, agentOf(project), key))
}

// sealKey makes a backup's data key and seals it with key.
func (s *Server) sealKey(ctx context.Context, project, key, backup string) ([]byte, *cmek.Sealed, error) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, nil, err
	}
	sealed, err := s.opts.Keys.Seal(ctx, project, agentOf(project), key, dek, []byte(backup))
	if err != nil {
		return nil, nil, keyError("kms_key", err)
	}
	return dek, &sealed, nil
}

// openKey unwraps a backup's data key.
func (s *Server) openKey(ctx context.Context, project, backup string, sealed *cmek.Sealed) ([]byte, error) {
	if sealed == nil {
		return nil, nil
	}
	if s.opts.Keys == nil {
		return nil, apierror.FailedPrecondition("Backup %s is encrypted with %s, and Cloud KMS is not served", backup, sealed.Key())
	}
	dek, err := s.opts.Keys.Open(ctx, project, agentOf(project), *sealed, []byte(backup))
	if err != nil {
		e := cmek.As(err)
		if e == nil {
			return nil, err
		}
		// UNVERIFIED wording (re-read 2026-11-23:
		// https://docs.cloud.google.com/filestore/docs/cmek).
		return nil, apierror.FailedPrecondition("Backup %s cannot be read: its key %s cannot be used: %s", backup, e.Key, e.Msg)
	}
	return dek, nil
}

// checkKeys suspends each READY instance whose key cannot be used, and
// resumes each it suspended once its key can be used again.
func (s *Server) checkKeys(ctx context.Context) {
	if s.opts.Keys == nil || s.opts.Backend == nil {
		return
	}
	type job struct {
		r       *instRecord
		project string
		key     string
	}
	s.mu.Lock()
	var jobs []job
	for name, r := range s.instances {
		k := r.I.GetKmsKeyName()
		st := r.I.GetState()
		if k == "" || s.busy[name] != "" || (st != filestorepb.Instance_READY && st != filestorepb.Instance_SUSPENDED) {
			continue
		}
		jobs = append(jobs, job{r, projectOf(name), k})
	}
	s.mu.Unlock()
	for _, j := range jobs {
		err := s.opts.Keys.Usable(ctx, j.project, agentOf(j.project), j.key)
		if e := cmek.As(err); err != nil && (e == nil || e.Kind == cmek.Unavailable) {
			// Cloud KMS is not answering: nothing is known of the key.
			continue
		}
		s.mu.Lock()
		st, name := j.r.I.GetState(), j.r.I.GetName()
		if s.busy[name] != "" || s.instances[name] != j.r {
			s.mu.Unlock()
			continue
		}
		suspend := err != nil && st == filestorepb.Instance_READY
		resume := err == nil && st == filestorepb.Instance_SUSPENDED && suspendedFor(j.r.I, filestorepb.Instance_KMS_KEY_ISSUE)
		if !suspend && !resume {
			s.mu.Unlock()
			continue
		}
		spec, key, ip := j.r.spec(), j.r.Key, j.r.IP
		if suspend {
			spec.Shares = nil
		}
		s.busy[name] = "key check"
		s.mu.Unlock()
		_, berr := s.opts.Backend.Ensure(ctx, key, ip, spec)
		s.mu.Lock()
		delete(s.busy, name)
		if berr == nil {
			if suspend {
				j.r.I.State, j.r.I.SuspensionReasons = filestorepb.Instance_SUSPENDED, []filestorepb.Instance_SuspensionReason{filestorepb.Instance_KMS_KEY_ISSUE}
				j.r.I.StatusMessage = "The KMS key used by the instance is either revoked or denied access to: " + cmek.As(err).Msg
			} else {
				j.r.I.State, j.r.I.SuspensionReasons, j.r.I.StatusMessage = filestorepb.Instance_READY, nil, ""
			}
			_ = s.saveLocked()
		}
		s.mu.Unlock()
	}
}

func suspendedFor(in *filestorepb.Instance, reason filestorepb.Instance_SuspensionReason) bool {
	for _, r := range in.GetSuspensionReasons() {
		if r == reason {
			return true
		}
	}
	return false
}
