package orgpolicy

import (
	"strings"
	"sync"
)

// Documented fields (#1895). Each service's custom constraints page lists
// the fields of each resource type a condition may read ("Supported
// resources", e.g. pubsub.googleapis.com/Schema: resource.definition,
// resource.name, resource.type). A resource type registered here is
// evaluated on those fields alone: any other field of its REST
// representation is absent, so a condition that reads one cannot be
// evaluated, which refuses the request as an unevaluable condition does.
// A type with no registered list is evaluated on its whole
// representation.
//
// A path names a field by its REST JSON names from the resource down,
// without "resource." ("bigqueryConfig.table"). A path that ends at a
// map or a message keeps it whole (resource.labels); one that passes
// through a list applies to every element.

var (
	fieldsMu     sync.RWMutex
	fieldsByType = map[string][]string{}
)

// RegisterFields records the fields Google documents for resourceType.
// Services call it from an init function of their own file here.
func RegisterFields(resourceType string, paths ...string) {
	fieldsMu.Lock()
	defer fieldsMu.Unlock()
	fieldsByType[resourceType] = append(fieldsByType[resourceType], paths...)
}

// RegisterTyped records the fields of resourceType whose JSON strings are
// a google.protobuf.Duration ("duration") or Timestamp ("timestamp"),
// which conditions compare with duration('30h') and timestamp('...').
func RegisterTyped(resourceType string, kinds map[string]string) {
	fieldsMu.Lock()
	defer fieldsMu.Unlock()
	m := typedFields[resourceType]
	if m == nil {
		m = map[string]string{}
		typedFields[resourceType] = m
	}
	for k, v := range kinds {
		m[k] = v
	}
}

// Fields are the documented fields of resourceType, nil when none are
// registered.
func Fields(resourceType string) []string {
	fieldsMu.RLock()
	defer fieldsMu.RUnlock()
	return append([]string(nil), fieldsByType[resourceType]...)
}

// restrict is obj with only the fields paths name.
func restrict(obj map[string]any, paths []string) map[string]any {
	tree := map[string]any{} // a path's segments, nil at a kept leaf
	for _, p := range paths {
		p = strings.TrimPrefix(p, "resource.")
		node := tree
		parts := strings.Split(p, ".")
		for i, seg := range parts {
			if i == len(parts)-1 {
				node[seg] = nil
				break
			}
			next, ok := node[seg].(map[string]any)
			if !ok {
				if _, leaf := node[seg]; leaf {
					break // a shorter path already keeps it whole
				}
				next = map[string]any{}
				node[seg] = next
			}
			node = next
		}
	}
	out, _ := prune(obj, tree).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

func prune(v any, tree map[string]any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, sub := range tree {
			x, ok := t[k]
			if !ok {
				continue
			}
			if sub == nil {
				out[k] = x
				continue
			}
			if p := prune(x, sub.(map[string]any)); p != nil {
				out[k] = p
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, x := range t {
			out = append(out, prune(x, tree))
		}
		return out
	}
	return nil // a scalar where the path goes deeper
}
