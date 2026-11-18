package iampolicy

import (
	"encoding/json"
	"net/http"
)

// RewriteMemberPath is where the host sends a deleted service account's
// binding rewrite (#1751) to a service that keeps its resources' IAM
// policies in another process (the storage server, the BigQuery, Pub/Sub,
// Bigtable and Spanner fronts): POST {"from": M, "to": N}, N empty to drop
// M. The service answers 204 once every policy naming M is rewritten.
const RewriteMemberPath = "/_cloudburrow/iam-rewrite-member"

// MemberRewrite is RewriteMemberPath's body.
type MemberRewrite struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ServeRewriteMember answers a POST to RewriteMemberPath with fn.
func ServeRewriteMember(w http.ResponseWriter, r *http.Request, fn func(from, to string) error) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST "+RewriteMemberPath+" rewrites a member", http.StatusMethodNotAllowed)
		return
	}
	var m MemberRewrite
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil || m.From == "" {
		http.Error(w, "the body is {\"from\": MEMBER, \"to\": MEMBER}", http.StatusBadRequest)
		return
	}
	if err := fn(m.From, m.To); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RewriteMemberIn rewrites the member in every policy of ps, reporting
// whether any changed.
func RewriteMemberIn[K comparable](ps map[K]*Stored, from, to string) bool {
	changed := false
	for _, p := range ps {
		if p.RewriteMember(from, to) {
			changed = true
		}
	}
	return changed
}
