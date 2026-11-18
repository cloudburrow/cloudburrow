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
// the organization, service accounts and tags. Other services' own stored
// policies (a bucket's, a topic's) keep the member as it was set.

import (
	"crypto/rand"
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
		changed := false
		kept := s.Bindings[:0]
		for _, bd := range s.Bindings {
			members := bd.Members[:0]
			for _, m := range bd.Members {
				if m != from {
					members = append(members, m)
					continue
				}
				changed = true
				if to != "" && !contains(members, to) {
					members = append(members, to)
				}
			}
			bd.Members = members
			if len(bd.Members) > 0 {
				kept = append(kept, bd)
			}
		}
		if !changed {
			continue
		}
		s.Bindings = kept
		s.Etag = make([]byte, 8)
		_, _ = rand.Read(s.Etag)
		out, err := json.Marshal(&s)
		if err != nil {
			return apierror.Internal(err, "encode the IAM policy %s", k)
		}
		if err := r.st.Put(k, out); err != nil {
			return apierror.Internal(err, "store the IAM policy %s", k)
		}
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
