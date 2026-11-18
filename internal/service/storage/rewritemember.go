package storage

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// rewriteMember replaces the member from with to (dropping it, and a
// binding left without members, when to is empty), numbering a changed
// policy's next set; it reports whether p changed. A deleted service
// account's bindings go through it (#1751): Google shows the account as
// deleted:serviceAccount:EMAIL?uid=ID in every policy that names it,
// back on undelete, gone on purge.
func (p *bucketPolicy) rewriteMember(from, to string) bool {
	if p == nil {
		return false
	}
	changed := false
	var kept []policyBinding
	for _, b := range p.Bindings {
		if !slices.Contains(b.Members, from) {
			kept = append(kept, b)
			continue
		}
		changed = true
		var members []string
		for _, m := range b.Members {
			if m == from {
				m = to
			}
			if m != "" && !slices.Contains(members, m) {
				members = append(members, m)
			}
		}
		if len(members) > 0 {
			b.Members = members
			kept = append(kept, b)
		}
	}
	if changed {
		p.Bindings = kept
		p.Seq++
	}
	return changed
}

// RewriteIAMMember rewrites the member in every bucket's and managed
// folder's policy, as the host asks at iampolicy.RewriteMemberPath.
func (s *Server) RewriteIAMMember(from, to string) error {
	return s.meta.Update(func(tx Tx) error {
		for _, prefix := range []string{bucketPrefix, managedFolderPrefix} {
			for _, key := range tx.List(prefix) {
				raw, ok := tx.Get(key)
				if !ok {
					continue
				}
				var rec map[string]json.RawMessage
				if json.Unmarshal(raw, &rec) != nil || rec["policy"] == nil {
					continue
				}
				var p bucketPolicy
				if json.Unmarshal(rec["policy"], &p) != nil || !p.rewriteMember(from, to) {
					continue
				}
				b, err := json.Marshal(p)
				if err != nil {
					return err
				}
				rec["policy"] = b
				out, err := json.Marshal(rec)
				if err != nil {
					return err
				}
				tx.Put(key, out)
			}
		}
		return nil
	})
}

func (s *Server) serveRewriteMember(w http.ResponseWriter, r *http.Request) {
	iampolicy.ServeRewriteMember(w, r, s.RewriteIAMMember)
}
