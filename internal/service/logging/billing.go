package logging

import (
	"sort"
	"strings"
)

// BillingAccounts are the billing accounts Logging holds anything for: an
// entry written to one of its logs, or stored configuration (a bucket, view,
// sink, exclusion or settings) under it. Cloud Billing is not served, so
// nothing else on the instance names a billing account; the console's log
// bucket screen offers these in its scope picker (#1411).
func (s *Server) BillingAccounts() []string {
	seen := map[string]bool{}
	for _, e := range s.store.snapshot() {
		if rest, ok := strings.CutPrefix(e.GetLogName(), collBilling+"/"); ok {
			if id, _, _ := strings.Cut(rest, "/"); id != "" {
				seen[collBilling+"/"+id] = true
			}
		}
	}
	if s.admin != nil {
		keys, _ := s.admin.db.List("logging/")
		marker := "/~" + collBilling + "~"
		for _, k := range keys {
			if i := strings.Index(k, marker); i >= 0 {
				id, _, _ := strings.Cut(k[i+len(marker):], "/")
				if id != "" {
					seen[collBilling+"/"+id] = true
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
