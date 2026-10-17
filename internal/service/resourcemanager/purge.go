package resourcemanager

// Purging a project (#1537).
//
// Google frees a project's ID once the project is purged, 30 days after
// DeleteProject or at once when it is shut down for good, and a project
// created again under that ID starts with nothing: no IAM policy, no org
// policy, no tags, no liens, no enabled services, no secrets. Until #1537
// the registry dropped the project's record and its IAM policy and nothing
// else, so a project made again under the ID read the old one's org
// policies and tag bindings, and every other service's resources under it.
//
// What the registry keeps for a project goes when it is purged, here; what
// the other services keep goes through the hooks OnPurge registers, which
// cmd/cloudburrow points at each service's per-project reset. While the
// project is DELETE_REQUESTED nothing is touched, so UndeleteProject gets
// it back whole, as on Google.

import (
	"errors"
	"log/slog"
	"strings"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// OnPurge registers fn to be called with a project's ID after the project is
// purged, for the state other services keep under it. It is called without
// the registry's lock held, before the call that purged the project returns,
// so fn may call back into the registry. A purge found on reading a project
// (RetentionPeriod after its deletion) logs fn's error; Purge returns it.
func (r *Registry) OnPurge(fn func(projectID string) error) {
	r.mu.Lock()
	defer r.unlock()
	r.onPurge = append(r.onPurge, fn)
}

// unlock releases r.mu, then tells the OnPurge hooks about every project
// purged while it was held. Every method that takes r.mu releases it here.
func (r *Registry) unlock() {
	purged, hooks := r.purged, r.onPurge
	r.purged = nil
	var numberHooks []func()
	if r.numbersChanged || len(purged) > 0 {
		numberHooks = r.onNumbers
	}
	r.numbersChanged = false
	r.mu.Unlock()
	defer func() {
		for _, fn := range numberHooks {
			fn()
		}
	}()
	for _, id := range purged {
		if err := runHooks(hooks, id); err != nil {
			slog.Warn("project purged, but not all of its state could be deleted", "project", id, "error", err)
		}
	}
}

func runHooks(hooks []func(string) error, id string) error {
	var errs []error
	for _, fn := range hooks {
		if err := fn(id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Purge purges a project pending deletion now, rather than RetentionPeriod
// after its deletion: the admin API's (POST /admin/purge), standing in for
// the wait. An ACTIVE project is FAILED_PRECONDITION: it must be deleted
// first, as on Google, where only a deleted project is purged. The record
// is gone whatever the hooks return; their error says what was left.
func (r *Registry) Purge(id string) error {
	r.mu.Lock()
	p, err := r.load(id)
	if err != nil {
		r.unlock()
		return err
	}
	if p.State != StateDeleteRequested {
		r.unlock()
		return apierror.FailedPrecondition("project %q is not pending deletion; delete it before purging it", id)
	}
	r.retireNumberLocked(p)
	r.numbersChanged = true
	_ = r.st.Delete(key(id))
	err = r.dropProjectStateLocked(id)
	hooks := r.onPurge
	r.unlock()
	return errors.Join(err, runHooks(hooks, id))
}

// purgeLocked removes a project's record and what the registry keeps for it,
// and queues the OnPurge hooks. The caller holds r.mu.
func (r *Registry) purgeLocked(p Project) {
	id := p.ProjectID
	r.retireNumberLocked(p)
	_ = r.st.Delete(key(id))
	if err := r.dropProjectStateLocked(id); err != nil {
		slog.Warn("project purged, but not all of its Resource Manager state could be deleted", "project", id, "error", err)
	}
	r.purged = append(r.purged, id)
}

// ResetProjectState deletes what the registry keeps for a project, the
// project itself apart: its IAM policy, org policies, liens, tag bindings
// and the tag keys it parents. With project empty it deletes every
// project's, every folder's and the organization's (IAM policies, org
// policies, liens, tag keys, values, holds and bindings); projects and
// folders stay. It is /admin/reset's (component resourcemanager).
func (r *Registry) ResetProjectState(project string) error {
	r.mu.Lock()
	defer r.unlock()
	if project != "" {
		return r.dropProjectStateLocked(project)
	}
	for _, prefix := range []string{iamPrefix, orgPolicyPrefix, folderOrgPolicyPrefix, orgOrgPolicyPrefix,
		lienPrefix, tagBindingPrefix, tagHoldPrefix, tagValuePrefix, tagKeyPrefix} {
		keys, err := r.st.List(prefix)
		if err != nil {
			return apierror.Internal(err, "list %s", prefix)
		}
		for _, k := range keys {
			if err := r.st.Delete(k); err != nil {
				return apierror.Internal(err, "delete %s", k)
			}
		}
	}
	return nil
}

// dropProjectStateLocked deletes what the registry keeps for project id
// other than its record. It does not read the record, which a purge has
// already deleted: a project is matched by its ID and its number. The
// caller holds r.mu.
func (r *Registry) dropProjectStateLocked(id string) error {
	r.dropIAM("projects/" + id)
	if err := r.deletePrefixLocked(orgPolicyPrefix + id + "/"); err != nil {
		return err
	}
	// Liens on it.
	lienKeys, err := r.st.List(lienPrefix)
	if err != nil {
		return apierror.Internal(err, "list liens")
	}
	for _, k := range lienKeys {
		if l, err := r.getLien(strings.TrimPrefix(k, lienPrefix)); err == nil && r.namesProject(l.Parent, id) {
			_ = r.st.Delete(k)
		}
	}
	// The tag keys it parents, their values, and those values' policies and
	// holds. Their namespace is the project's ID.
	values := map[string]bool{}
	keys, err := listProtos(r, tagKeyPrefix, func() *rmpb.TagKey { return &rmpb.TagKey{} })
	if err != nil {
		return err
	}
	for _, k := range keys {
		if !strings.HasPrefix(k.GetNamespacedName(), id+"/") {
			continue
		}
		vals, err := listProtos(r, tagValuePrefix, func() *rmpb.TagValue { return &rmpb.TagValue{} })
		if err != nil {
			return err
		}
		for _, v := range vals {
			if v.GetParent() != k.GetName() {
				continue
			}
			values[v.GetName()] = true
			vid := strings.TrimPrefix(v.GetName(), "tagValues/")
			if err := r.deletePrefixLocked(tagHoldPrefix + vid + "/"); err != nil {
				return err
			}
			_ = r.st.Delete(tagValuePrefix + vid)
			r.dropIAM(v.GetName())
		}
		_ = r.st.Delete(tagKeyPrefix + strings.TrimPrefix(k.GetName(), "tagKeys/"))
		r.dropIAM(k.GetName())
	}
	// Bindings on it or on a resource in it, and bindings of its values.
	bkeys, err := r.st.List(tagBindingPrefix)
	if err != nil {
		return apierror.Internal(err, "list tag bindings")
	}
	for _, bk := range bkeys {
		b := &rmpb.TagBinding{}
		if err := r.getProto(bk, b); err != nil {
			continue
		}
		if values[b.GetTagValue()] || r.inProject(b.GetParent(), id) {
			_ = r.st.Delete(bk)
		}
	}
	return nil
}

// inProject reports whether a full resource name is project id's, by ID or
// number: "//cloudresourcemanager.googleapis.com/projects/{id}", or another
// service's "//{service}/projects/{id}/...".
func (r *Registry) inProject(fullName, id string) bool {
	_, rest, ok := strings.Cut(strings.TrimPrefix(fullName, "//"), "/projects/")
	if !ok {
		return false
	}
	x, _, _ := strings.Cut(rest, "/")
	return r.namesProject("projects/"+x, id)
}

// deletePrefixLocked deletes every record under prefix. The caller holds r.mu.
func (r *Registry) deletePrefixLocked(prefix string) error {
	keys, err := r.st.List(prefix)
	if err != nil {
		return apierror.Internal(err, "list %s", prefix)
	}
	for _, k := range keys {
		if err := r.st.Delete(k); err != nil {
			return apierror.Internal(err, "delete %s", k)
		}
	}
	return nil
}
