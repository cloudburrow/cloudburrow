package kms

import (
	"errors"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// RewriteIAMMember replaces the member from with to (dropping it when to
// is empty) in the policy of every key ring, key, EKM connection and EKM
// config: a deleted service account's bindings (#1751), which the resource
// manager registry's OnMemberRewrite hook passes here.
func (s *Server) RewriteIAMMember(from, to string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, prefix := range []string{ringPrefix, keyPrefix, ekmConnectionPrefix, ekmConfigPrefix} {
		keys, err := s.db.List(prefix)
		if err != nil {
			return apierror.Internal(err, "list %s", prefix)
		}
		for _, k := range keys {
			raw, err := s.db.Get(k)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return apierror.Internal(err, "read %s", k)
			}
			out, changed, err := iampolicy.RewriteMemberInRecord(raw, "iamPolicy", from, to)
			if err != nil {
				return apierror.Internal(err, "decode %s", k)
			}
			if changed {
				if err := s.db.Put(k, out); err != nil {
					return apierror.Internal(err, "store %s", k)
				}
			}
		}
	}
	return nil
}
