package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// A Cloud Run workload's service account (#1388).
//
// On Cloud Run the metadata server a revision, a job's task or a worker
// pool's instance reaches names the service account it runs as: the
// template's serviceAccount, or the project's Compute Engine default account
// when none is set. The email, an identity token's email claim and the
// account an access token is for are all that account's. Nothing checks
// any of it here (ADR-0004); what changes is only what a workload is told.
//
// Every workload reaches the one metadata server, through cloudburrow-host,
// and arrives from an address that says nothing about it: the pod's traffic
// is translated on its way out of the cluster, to the host. So the account
// is carried by the name the workload dials. Each account has an
// ExternalName Service, cloudburrow-sa-<hash>, in the workload namespace,
// whose DNS name is an alias of cloudburrow-host's, and a workload's
// GCE_METADATA_HOST names it in place of cloudburrow-host. Every client
// library sends that name as Host, and the metadata server asks this adapter
// which account it stands for. The Service's annotation records the email,
// so the answer survives a restart of the CLI.
//
// One Service per account, not per workload: a revision keeps the account it
// was deployed with while a later one serves another, and a Service never
// has to be deleted from under a workload still using it.

// annMetadataAccount is the alias Service's annotation naming its account.
const annMetadataAccount = "run.cloudburrow.dev/service-account"

// labelMetadataAlias marks an alias Service.
const labelMetadataAlias = "run.cloudburrow.dev/metadata-alias"

// DefaultComputeAccount is the account a Cloud Run workload with no
// serviceAccount runs as: the project's Compute Engine default service
// account, {number}-compute@developer.gserviceaccount.com, with the number
// every service reports for the project.
func DefaultComputeAccount(project string) string {
	return fmt.Sprintf("%d-compute@developer.gserviceaccount.com", resource.ProjectNumber(project))
}

// workloadAccount is the account a workload in project runs as.
func workloadAccount(project, serviceAccount string) string {
	if serviceAccount != "" {
		return serviceAccount
	}
	return DefaultComputeAccount(project)
}

// metadataAliasLabel is the Service name, and the host's first label, for an
// account: a DNS label however long the email is.
func metadataAliasLabel(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(email)))
	return metadata.AccountHostPrefix + hex.EncodeToString(sum[:])[:16]
}

// metadataAliases is what the adapter knows of its alias Services.
type metadataAliases struct {
	mu sync.Mutex
	// byLabel is each alias's email.
	byLabel map[string]string
}

func (a *metadataAliases) remember(label, email string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.byLabel == nil {
		a.byLabel = map[string]string{}
	}
	a.byLabel[label] = email
}

func (a *metadataAliases) lookup(label string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.byLabel[label]
	return e, ok
}

// MetadataAccount is the account a workload metadata host's first label
// stands for: the metadata server's metadata.AccountResolver. A label this
// process has not seen is read from its Service, which a previous run made.
func (s *Server) MetadataAccount(label string) (string, bool) {
	if !strings.HasPrefix(label, metadata.AccountHostPrefix) {
		return "", false
	}
	if e, ok := s.aliases.lookup(label); ok {
		return e, true
	}
	if s.kn == nil || s.kn.Kube == nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := s.kn.Kube.Get(ctx, "service", label, "json")
	if err != nil {
		return "", false
	}
	var svc struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
			Labels      map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(out), &svc) != nil || svc.Metadata.Labels[labelMetadataAlias] != "true" {
		return "", false
	}
	email := svc.Metadata.Annotations[annMetadataAccount]
	if email == "" || metadataAliasLabel(email) != label {
		return "", false
	}
	s.aliases.remember(label, email)
	return email, true
}

// withWorkloadMetadata points the injected GCE_METADATA_HOST at the
// account's alias of the same server, on the same port.
func withWorkloadMetadata(injected []injectedEnv, namespace, email string) []injectedEnv {
	out := make([]injectedEnv, len(injected))
	copy(out, injected)
	for i, e := range out {
		if e.name != "GCE_METADATA_HOST" {
			continue
		}
		_, port, err := net.SplitHostPort(e.value)
		if err != nil {
			continue
		}
		out[i].value = net.JoinHostPort(metadataAliasLabel(email)+"."+namespace+".svc.cluster.local", port)
	}
	return out
}

// ensureMetadataAlias applies the account's alias Service, pointing at the
// host the injected GCE_METADATA_HOST names. It is applied before every
// workload that uses it, so the name resolves by the time the workload's
// first container asks for a token, and an alias removed with its
// namespace is put back by the next deploy rather than remembered as there.
func (s *Server) ensureMetadataAlias(ctx context.Context, target, email string) error {
	label := metadataAliasLabel(email)
	s.aliases.remember(label, email)
	if s.kn == nil || s.kn.Kube == nil {
		return nil
	}
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
  labels:
    %s: %q
    %s: %q
    %s: "true"
  annotations:
    %s: %q
spec:
  type: ExternalName
  externalName: %s
`, label, s.kn.namespace(), labelOwned, k8s.OwnedValue, labelInstance, s.instance, labelMetadataAlias,
		annMetadataAccount, email, target)
	if err := s.kn.Kube.Apply(ctx, manifest, k8s.ApplyOptions{}); err != nil {
		return apierror.Internal(err, "apply the metadata server's name for %s", email)
	}
	return nil
}
