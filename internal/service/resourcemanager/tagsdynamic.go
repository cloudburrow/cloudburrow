package resourcemanager

// Dynamic tag values (#2023), as Google documents them ("Creating and
// managing tags", Dynamic values, read 2026-11-20, and the v3 TagKey
// resource's allowedValuesRegex): a tag key whose allowedValuesRegex is set
// takes its values when a tag is attached, not from predefined TagValues.
//
//   - The regex is RE2 syntax (Go's regexp is RE2); a value matches when the
//     whole value matches it, so ".*" allows any value.
//   - A key with dynamic values can't also have predefined values: creating
//     a TagValue under it is FAILED_PRECONDITION, and so is setting the
//     regex on a key that has values.
//   - A dynamic value is attached by its namespaced name only
//     ({namespace}/{key short name}/{value}), since it has no ID; the
//     binding is tagBindings/{full resource name}/tagKeys/{id}, as the
//     TagBinding name's second documented form says. A value that does not
//     match is INVALID_ARGUMENT.
//   - Resources inherit dynamic bindings like any other (ListEffectiveTags).
//   - The description and the regex can be updated; a new regex does not
//     apply retroactively, so existing bindings stay.
//   - All of the key's bindings must be deleted before the key is.
//
// The field exists in the REST surface only: Google's discovery document
// (cloudresourcemanager v3, revision 20261107) has allowedValuesRegex, the
// published gRPC TagKey message (cloud.google.com/go/resourcemanager
// v1.18.1, the newest) has no such field, so the regex is stored beside the
// TagKey record and the JSON handlers carry it (tags_api.go).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const tagKeyRegexPrefix = "resourcemanager/tagkeyregex/"

// maxDynamicValueLength bounds a dynamic value as a TagValue short name is
// bounded.
const maxDynamicValueLength = 256

// compileAllowedValuesRegex checks an allowedValuesRegex and returns the
// matcher for whole values.
func compileAllowedValuesRegex(re string) (*regexp.Regexp, error) {
	m, err := regexp.Compile(`^(?:` + re + `)$`)
	if err != nil {
		return nil, apierror.InvalidArgument("allowed_values_regex %q is not a valid RE2 regular expression: %v", re, err)
	}
	return m, nil
}

// tagKeyRegexLocked is the key's allowedValuesRegex, "" for a key of
// predefined values. The caller holds r.mu.
func (r *Registry) tagKeyRegexLocked(id string) string {
	b, err := r.st.Get(tagKeyRegexPrefix + id)
	if err != nil {
		return ""
	}
	return string(b)
}

// TagKeyAllowedValuesRegex is the allowedValuesRegex of the key name
// (tagKeys/{id}), "" for a key of predefined values or none.
func (r *Registry) TagKeyAllowedValuesRegex(name string) string {
	id, err := numericID("tagKeys/", name)
	if err != nil {
		return ""
	}
	r.mu.Lock()
	defer r.unlock()
	return r.tagKeyRegexLocked(id)
}

func (r *Registry) putTagKeyRegexLocked(id, re string) error {
	if re == "" {
		if _, err := r.st.Get(tagKeyRegexPrefix + id); err != nil {
			return nil
		}
		if err := r.st.Delete(tagKeyRegexPrefix + id); err != nil {
			return apierror.Internal(err, "clear allowed_values_regex")
		}
		return nil
	}
	if err := r.st.Put(tagKeyRegexPrefix+id, []byte(re)); err != nil {
		return apierror.Internal(err, "store allowed_values_regex")
	}
	return nil
}

// tagKeyEtag is a key's etag: its record's digest, and its regex's.
func tagKeyEtag(k *rmpb.TagKey, re string) string {
	e := protoEtag(k)
	if re == "" {
		return e
	}
	return policyEtag([]byte(e + "\x00" + re))
}

// CreateTagKeyWithRegex is CreateTagKey with an allowedValuesRegex: a key of
// dynamic values when re is set.
func (r *Registry) CreateTagKeyWithRegex(in *rmpb.TagKey, re string, validateOnly bool) (*rmpb.TagKey, error) {
	if re != "" {
		if _, err := compileAllowedValuesRegex(re); err != nil {
			return nil, err
		}
	}
	return r.createTagKey(in, re, validateOnly)
}

// keyOfBindingLocked is the tag key a binding attaches a value of: its
// TagValue's parent, or, for a dynamic value, the key its name ends in.
func (r *Registry) keyOfBindingLocked(b *rmpb.TagBinding) string {
	if b.GetTagValue() != "" {
		if v, _, err := r.tagValueLocked(b.GetTagValue()); err == nil {
			return v.GetParent()
		}
		return ""
	}
	if i := strings.LastIndex(b.GetName(), "/tagKeys/"); i >= 0 {
		return b.GetName()[i+1:]
	}
	return ""
}

// dynamicValueLocked resolves a namespaced name that names no predefined
// value: a dynamic value of a key with an allowedValuesRegex. It returns
// the key, or an error that says why the value is refused.
func (r *Registry) dynamicValueLocked(namespaced string) (*rmpb.TagKey, error) {
	parts := strings.SplitN(namespaced, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return nil, apierror.InvalidArgument("tag value namespaced name %q must be {parent_id}/{tag_key_short_name}/{short_name}", namespaced)
	}
	keys, err := listProtos(r, tagKeyPrefix, func() *rmpb.TagKey { return &rmpb.TagKey{} })
	if err != nil {
		return nil, err
	}
	var k *rmpb.TagKey
	for _, x := range keys {
		if x.GetNamespacedName() == parts[0]+"/"+parts[1] {
			k = x
		}
	}
	if k == nil {
		return nil, apierror.NotFound("tag value %s not found", namespaced)
	}
	re := r.tagKeyRegexLocked(idOf(k.GetName()))
	if re == "" {
		return nil, apierror.NotFound("tag value %s not found", namespaced)
	}
	value := parts[2]
	if value == "" || len(value) > maxDynamicValueLength {
		return nil, apierror.InvalidArgument("dynamic tag value %q must be 1 to %d characters", value, maxDynamicValueLength)
	}
	m, err := compileAllowedValuesRegex(re)
	if err != nil {
		return nil, err
	}
	if !m.MatchString(value) {
		return nil, apierror.InvalidArgument("tag value %q does not match the validation regular expression %q of tag key %s (allowed_values_regex)",
			value, re, k.GetNamespacedName())
	}
	return k, nil
}

// dynamicBindingsOfLocked counts the bindings of key's dynamic values.
func (r *Registry) dynamicBindingsOfLocked(key string) (int, error) {
	all, err := listProtos(r, tagBindingPrefix, func() *rmpb.TagBinding { return &rmpb.TagBinding{} })
	if err != nil {
		return 0, err
	}
	n := 0
	for _, b := range all {
		if b.GetTagValue() == "" && r.keyOfBindingLocked(b) == key {
			n++
		}
	}
	return n, nil
}

// --- JSON: allowedValuesRegex ----------------------------------------------

// readTagKeyJSON decodes a REST TagKey, taking allowedValuesRegex out of the
// body for the registry; set says whether the body named it.
func readTagKeyJSON(req *http.Request) (k *rmpb.TagKey, re string, set bool, err error) {
	k = &rmpb.TagKey{}
	b, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		return nil, "", false, apierror.InvalidArgument("read body: %v", err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return k, "", false, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, "", false, apierror.InvalidArgument("decode body: %v", err)
	}
	for _, f := range []string{"allowedValuesRegex", "allowed_values_regex"} {
		if v, ok := raw[f]; ok {
			if err := json.Unmarshal(v, &re); err != nil {
				return nil, "", false, apierror.InvalidArgument("decode body: allowedValuesRegex must be a string")
			}
			set = true
			delete(raw, f)
		}
	}
	rest, err := json.Marshal(raw)
	if err != nil {
		return nil, "", false, apierror.InvalidArgument("decode body: %v", err)
	}
	if err := (protojson.UnmarshalOptions{}).Unmarshal(rest, k); err != nil {
		return nil, "", false, apierror.InvalidArgument("decode body: %v", err)
	}
	return k, re, set, nil
}

// writeTagKeyJSON writes m, a TagKey, a list of them or an operation whose
// response is one, with each key's allowedValuesRegex added: the registry's,
// or extra's for a key not stored (validateOnly).
func (r *Registry) writeTagKeyJSON(w http.ResponseWriter, m proto.Message, extra map[string]string) error {
	b, err := pj.Marshal(m)
	if err != nil {
		return apierror.Internal(err, "encode response")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return apierror.Internal(err, "encode response")
	}
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			if name, ok := t["name"].(string); ok && strings.HasPrefix(name, "tagKeys/") {
				if _, isKey := t["shortName"]; isKey {
					re, ok := extra[name]
					if !ok {
						re = r.TagKeyAllowedValuesRegex(name)
					}
					if re != "" {
						t["allowedValuesRegex"] = re
					}
				}
			}
			for _, c := range t {
				walk(c)
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		}
	}
	walk(v)
	out, err := json.Marshal(v)
	if err != nil {
		return apierror.Internal(err, "encode response")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, err = w.Write(out)
	return err
}
