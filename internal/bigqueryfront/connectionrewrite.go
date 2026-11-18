package bigqueryfront

import "encoding/json"

// rewriteMember replaces the member from with to (dropping it when to is
// empty, and a binding left with no member) in every connection's IAM
// policy: a deleted service account's bindings (#1795), which the host
// sends to iampolicy.RewriteMemberPath with the tables' and datasets'
// (iam.go). Google shows serviceAccount:EMAIL as
// deleted:serviceAccount:EMAIL?uid=ID in every allow policy that names it,
// turns it back on undelete and drops it on purge
// (https://docs.cloud.google.com/iam/docs/allow-policies). A changed
// policy gets a new etag; its roles and conditions are kept.
func (c *bqConnections) rewriteMember(from, to string) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := false
	for _, cn := range c.conns {
		if len(cn.Policy) == 0 {
			continue
		}
		var p map[string]any
		if json.Unmarshal(cn.Policy, &p) != nil {
			continue
		}
		bindings, _ := p["bindings"].([]any)
		var kept []any
		touched := false
		for _, raw := range bindings {
			b, ok := raw.(map[string]any)
			if !ok {
				kept = append(kept, raw)
				continue
			}
			members, _ := b["members"].([]any)
			var out []any
			seen := map[string]bool{}
			hit := false
			for _, m := range members {
				s, _ := m.(string)
				if s == from {
					s, hit = to, true
				}
				if s == "" || seen[s] {
					continue
				}
				seen[s] = true
				out = append(out, s)
			}
			if !hit {
				kept = append(kept, raw)
				continue
			}
			touched = true
			if len(out) > 0 {
				b["members"] = out
				kept = append(kept, b)
			}
		}
		if !touched {
			continue
		}
		if kept == nil {
			delete(p, "bindings")
		} else {
			p["bindings"] = kept
		}
		cn.Etag++
		p["etag"] = cn.etagString()
		cn.Policy, _ = json.Marshal(p)
		changed = true
	}
	if changed {
		c.saveLocked()
	}
	return nil
}
