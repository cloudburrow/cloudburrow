package resourcemanager

// A deleted service account's bindings (#1750). Google's pages
// (docs.cloud.google.com/iam/docs/service-accounts-delete-undelete and
// .../principal-identifiers, read 2026-11-17): a deleted account's role
// bindings are not removed at once; the account "appears in role bindings
// with a deleted: prefix and a ?uid=NUMERIC_ID suffix", so a binding of
// serviceAccount:EMAIL reads deleted:serviceAccount:EMAIL?uid=UNIQUE_ID and
// grants nothing, not even to a new account created with the same email;
// an undeleted account "retains its roles", so undelete turns the
// identifier back. The bindings are purged after at most 60 days; here
// they go when the account record does, 30 days after deletion, when it
// can no longer be undeleted.
//
// The rewrite covers every policy the registry keeps: projects, folders,
// the organization, service accounts and tags; and, through the hooks
// OnMemberRewrite registers (#1751), the policies other services keep on
// their own resources (a secret's, a key ring's, a queue's), as Google's
// "appears in role bindings" covers every allow policy.

import (
	"encoding/json"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// deletedMember is a deleted account's identifier in a binding.
func deletedMember(a *serviceAccount) string {
	return "deleted:serviceAccount:" + a.email() + "?uid=" + a.UniqueID
}

// rewriteMemberLocked replaces member from with to (or drops it, when to
// is empty) in every policy the registry keeps; a policy it changes gets a
// new etag. The caller holds r.mu.
func (r *Registry) rewriteMemberLocked(from, to string) error {
	keys, err := r.st.List(iamPrefix)
	if err != nil {
		return apierror.Internal(err, "list IAM policies")
	}
	for _, k := range keys {
		b, err := r.st.Get(k)
		if err != nil {
			continue
		}
		var s iampolicy.Stored
		if err := json.Unmarshal(b, &s); err != nil {
			return apierror.Internal(err, "decode the IAM policy %s", k)
		}
		if !s.RewriteMember(from, to) {
			continue
		}
		out, err := json.Marshal(&s)
		if err != nil {
			return apierror.Internal(err, "encode the IAM policy %s", k)
		}
		if err := r.st.Put(k, out); err != nil {
			return apierror.Internal(err, "store the IAM policy %s", k)
		}
	}
	r.rewrites = append(r.rewrites, memberRewrite{from, to})
	return nil
}

// memberRewrite is one rewrite unlock passes to the OnMemberRewrite hooks.
type memberRewrite struct{ from, to string }

// OnMemberRewrite registers fn to be called when a deleted service
// account's bindings change: from serviceAccount:EMAIL to
// deleted:serviceAccount:EMAIL?uid=ID when it is deleted, back when it is
// undeleted, and from the deleted identifier to "" (drop it) when it is
// purged. fn rewrites the policies its service keeps on its own resources.
// It is called without the registry's lock held, before the call that
// changed the account returns; an error is logged.
func (r *Registry) OnMemberRewrite(fn func(from, to string) error) {
	r.mu.Lock()
	defer r.unlock()
	r.onRewrite = append(r.onRewrite, fn)
}
