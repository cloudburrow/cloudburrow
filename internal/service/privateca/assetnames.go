package privateca

import (
	"sort"
	"strings"
)

// PolicyResourceNames are the project's CA pools and certificate templates,
// in every location, by name: the resources that keep allow policies, for
// Cloud Asset's IAM policy analysis under a scope (#2112), as Cloud Asset
// Inventory's privateca.googleapis.com/CaPool and CertificateTemplate
// assets are named.
func (s *Server) PolicyResourceNames(project string) []string {
	prefix := "projects/" + project + "/locations/"
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for n := range s.pools {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	for n := range s.templates {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
