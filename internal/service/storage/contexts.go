package storage

import (
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Object contexts (#1345): key-value pairs under contexts.custom, each with
// a createTime and an updateTime, that a listing can filter on. Both APIs
// share this file: gRPC's Object.contexts and ListObjectsRequest.filter
// reach it through the JSON handlers (grpcconv.go).
//
// The rules are Google's (docs.cloud.google.com/storage/docs/object-contexts,
// use-object-contexts, listing-objects#filter-by-object-contexts, and the
// JSON API's discovery document):
//   - a key and a value are 1 to 256 bytes, begin with a letter or digit,
//     and hold no ' " \ or /; keys under google: are reserved;
//   - a patch sets the keys it names, a key set to null (or to a payload
//     with no value, which the official Go client sends) is deleted, and
//     contexts (or contexts.custom) set to null deletes them all; an update
//     replaces them; neither creates a new generation;
//   - copy, rewrite and compose transfer the source's contexts unless the
//     request body gives contexts (which override them all) or
//     dropContextGroups contains "custom" (which drops them);
//   - a listing's filter is one of contexts."KEY":*, contexts."KEY"="VALUE",
//     or either negated by NOT or a leading -.

// objectContext is one entry of contexts.custom.
type objectContext struct {
	Value   string    `json:"value"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

const maxContextBytes = 256

func checkContextText(what, s string) error {
	if s == "" || len(s) > maxContextBytes {
		return badRequest("Invalid argument: an object context %s must be between 1 and 256 characters, inclusive", what)
	}
	if r, _ := utf8.DecodeRuneInString(s); !unicode.IsLetter(r) && !unicode.IsDigit(r) {
		return badRequest("Invalid argument: an object context %s must begin with an alphanumeric character: %q", what, s)
	}
	if strings.ContainsAny(s, `'"\/`) {
		return badRequest("Invalid argument: an object context %s cannot contain a single quote, double quote, backslash or forward slash: %q", what, s)
	}
	return nil
}

func checkContextKey(k string) error {
	if err := checkContextText("key", k); err != nil {
		return err
	}
	if strings.HasPrefix(strings.ToLower(k), "google:") {
		return badRequest("Invalid argument: object context keys with the google: prefix are reserved: %q", k)
	}
	return nil
}

// checkContextsBody checks a body's contexts: null, or {"custom": null}, or
// {"custom": {KEY: null | {"value": VALUE, ...}}}.
func checkContextsBody(v any) error {
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return badRequest("Invalid argument: contexts must be an object")
	}
	for k := range m {
		if k != "custom" {
			return badRequest("Invalid argument: contexts.%s is not an ObjectContexts field", k)
		}
	}
	custom, ok := m["custom"]
	if !ok || custom == nil {
		return nil
	}
	cm, ok := custom.(map[string]any)
	if !ok {
		return badRequest("Invalid argument: contexts.custom must be an object")
	}
	for k, pv := range cm {
		if err := checkContextKey(k); err != nil {
			return err
		}
		if pv == nil {
			continue
		}
		p, ok := pv.(map[string]any)
		if !ok {
			return badRequest("Invalid argument: contexts.custom.%s must be an object", k)
		}
		for f := range p {
			switch f {
			case "value", "createTime", "updateTime":
			default:
				return badRequest("Invalid argument: contexts.custom.%s.%s is not an ObjectCustomContextPayload field", k, f)
			}
		}
		// A payload without a value deletes the key, as null does: the
		// official Go client's ObjectCustomContextPayload.Delete sends {}.
		rawVal, has := p["value"]
		if !has {
			continue
		}
		val, isStr := rawVal.(string)
		if !isStr {
			return badRequest("Invalid argument: contexts.custom.%s.value must be a string", k)
		}
		if err := checkContextText("value", val); err != nil {
			return err
		}
	}
	return nil
}

// applyContexts writes a checked body's contexts onto o: merged by a patch,
// replacing them for an update (or a copy's override). A key that keeps its
// value keeps its updateTime; createTime stays while the key does.
func applyContexts(o *objectRecord, v any, replace bool, now time.Time) {
	var custom map[string]any
	if m, ok := v.(map[string]any); ok {
		custom, _ = m["custom"].(map[string]any)
	}
	if custom == nil {
		o.Contexts = nil
		return
	}
	old := o.Contexts
	next := map[string]objectContext{}
	if !replace {
		for k, c := range old {
			next[k] = c
		}
	}
	for k, pv := range custom {
		p, _ := pv.(map[string]any)
		rawVal, has := p["value"]
		if !has {
			delete(next, k)
			continue
		}
		val := str(rawVal)
		c, had := old[k]
		switch {
		case !had:
			c = objectContext{Value: val, Created: now, Updated: now}
		case c.Value != val:
			c.Value, c.Updated = val, now
		}
		next[k] = c
	}
	if len(next) == 0 {
		next = nil
	}
	o.Contexts = next
}

// contextsJSON is o's contexts as the JSON API's ObjectContexts.
func contextsJSON(o objectRecord) map[string]any {
	custom := map[string]any{}
	for k, c := range o.Contexts {
		custom[k] = map[string]any{"value": c.Value, "createTime": rfc3339(c.Created), "updateTime": rfc3339(c.Updated)}
	}
	return map[string]any{"custom": custom}
}

// mergeContexts is the contexts a compose carries over: every source's, a
// later source's value winning for a key two of them share.
func mergeContexts(sources []objectRecord) map[string]objectContext {
	var out map[string]objectContext
	for _, s := range sources {
		for k, c := range s.Contexts {
			if out == nil {
				out = map[string]objectContext{}
			}
			out[k] = c
		}
	}
	return out
}

// dropCustomContexts reads dropContextGroups, whose only accepted value is
// "custom".
func dropCustomContexts(q url.Values, more ...string) (bool, error) {
	drop := false
	for _, g := range append(q["dropContextGroups"], more...) {
		if g != "custom" {
			return false, badRequest("Invalid argument: dropContextGroups %q: the accepted value is 'custom'", g)
		}
		drop = true
	}
	return drop, nil
}

// contextFilter is a parsed listing filter.
type contextFilter struct {
	key, value string
	any, not   bool
}

func (f contextFilter) match(o objectRecord) bool {
	c, has := o.Contexts[f.key]
	ok := has && (f.any || c.Value == f.value)
	return ok != f.not
}

// parseContextFilter parses the filter grammar Google documents: one term,
// contexts."KEY":* or contexts."KEY"="VALUE", optionally negated by NOT or
// a leading -. Anything else (AND, OR, other fields) is refused.
func parseContextFilter(s string) (*contextFilter, error) {
	in := strings.TrimSpace(s)
	if in == "" {
		return nil, nil
	}
	bad := badRequest("Invalid argument: the filter %q is not valid: only contexts.\"KEY\":*, contexts.\"KEY\"=\"VALUE\" and their negations (NOT or -) are supported", s)
	f := &contextFilter{}
	if rest, ok := strings.CutPrefix(in, "-"); ok {
		f.not, in = true, rest
	} else if rest, ok := strings.CutPrefix(in, "NOT "); ok {
		f.not, in = true, strings.TrimLeft(rest, " ")
	}
	in, ok := strings.CutPrefix(in, `contexts."`)
	if !ok {
		return nil, bad
	}
	i := strings.IndexByte(in, '"')
	if i <= 0 {
		return nil, bad
	}
	f.key, in = in[:i], strings.TrimSpace(in[i+1:])
	switch {
	case strings.TrimSpace(strings.TrimPrefix(in, ":")) == "*" && strings.HasPrefix(in, ":"):
		f.any = true
	case strings.HasPrefix(in, "="):
		v := strings.TrimSpace(in[1:])
		if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' || strings.Count(v, `"`) != 2 {
			return nil, bad
		}
		f.value = v[1 : len(v)-1]
	default:
		return nil, bad
	}
	return f, nil
}
