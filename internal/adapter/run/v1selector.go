package run

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	runv1 "google.golang.org/api/run/v1"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The labelSelector and fieldSelector of v1's list calls (#1420).
//
// labelSelector is Kubernetes' label selector grammar, which Cloud Run v1
// documents as "Supported operations are =, !=, exists, in, and notIn":
// comma-joined requirements, each one of
//
//	key=value  key==value  key!=value
//	key in (v1,v2)  key notin (v1,v2)
//	key  !key
//
// with spaces allowed around operators, as gcloud sends them ("key = value",
// and "run.googleapis.com/runningState in (Succeeded,Failed)" for
// gcloud run jobs executions tasks list --succeeded --failed). As in
// Kubernetes, != and notin match an object without the label. Anything else
// (Kubernetes' gt and lt, a malformed term) is INVALID_ARGUMENT.
//
// fieldSelector is documented as "Not supported by Cloud Run" on most v1
// list calls, and on services.list as "Allows to filter resources based on a
// specific value for a field name ... i.e. 'metadata.name%3Dlorem'". gcloud
// beta sends metadata.name={service} to projects.locations.services.list to
// find a service's region. It is served here with Kubernetes' field
// selector grammar (=, ==, !=, comma-joined) on the two fields every object
// has, metadata.name and metadata.namespace; any other field is
// INVALID_ARGUMENT, with the Kubernetes API server's wording ("field label
// not supported"). Whether Google filters on it or ignores it is
// UNVERIFIED; gcloud matches the name again itself.

// v1Requirement is one term of a selector.
type v1Requirement struct {
	key string
	// op is "=", "!=", "in", "notin", "exists" or "!".
	op     string
	values []string
}

// v1Selector is a list call's label and field selectors.
type v1Selector struct {
	labels, fields []v1Requirement
}

var (
	// A label key: an optional DNS-subdomain prefix and a name, as
	// Kubernetes allows; a value: a name or empty.
	v1LabelKeyRE   = regexp.MustCompile(`^([a-z0-9]([-a-z0-9.]*[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
	v1LabelValueRE = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$`)
)

// labelSelector parses a list request's labelSelector and fieldSelector.
func labelSelector(r *http.Request) (v1Selector, error) {
	q := r.URL.Query()
	var sel v1Selector
	var err error
	if sel.labels, err = parseLabelSelector(q.Get("labelSelector")); err != nil {
		return v1Selector{}, err
	}
	if sel.fields, err = parseFieldSelector(q.Get("fieldSelector")); err != nil {
		return v1Selector{}, err
	}
	return sel, nil
}

// splitSelector splits a selector at the commas outside parentheses.
func splitSelector(s string) ([]string, bool) {
	var out []string
	depth, start := 0, 0
	for i, c := range s {
		switch c {
		case '(':
			depth++
			if depth > 1 {
				return nil, false
			}
		case ')':
			depth--
			if depth < 0 {
				return nil, false
			}
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return nil, false
	}
	return append(out, s[start:]), true
}

// v1SetTermRE is a set-based term: key in (values) or key notin (values).
var v1SetTermRE = regexp.MustCompile(`^(\S+)\s+(?i:(in|notin))\s*\((.*)\)$`)

func parseLabelSelector(s string) ([]v1Requirement, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	bad := func(why string) error {
		return apierror.InvalidArgument("labelSelector %q: %s", s, why)
	}
	terms, ok := splitSelector(s)
	if !ok {
		return nil, bad("unbalanced parentheses")
	}
	var out []v1Requirement
	for _, term := range terms {
		term = strings.TrimSpace(term)
		var req v1Requirement
		switch {
		case term == "":
			return nil, bad("empty requirement")
		case v1SetTermRE.MatchString(term):
			m := v1SetTermRE.FindStringSubmatch(term)
			req = v1Requirement{key: m[1], op: strings.ToLower(m[2])}
			for _, v := range strings.Split(m[3], ",") {
				v = strings.TrimSpace(v)
				if !v1LabelValueRE.MatchString(v) {
					return nil, bad("invalid label value " + strconv.Quote(v))
				}
				req.values = append(req.values, v)
			}
		case strings.Contains(term, "!="):
			k, v, _ := strings.Cut(term, "!=")
			req = v1Requirement{key: strings.TrimSpace(k), op: "!=", values: []string{strings.TrimSpace(v)}}
		case strings.Contains(term, "="):
			k, v, _ := strings.Cut(term, "=")
			v = strings.TrimPrefix(v, "=")
			req = v1Requirement{key: strings.TrimSpace(k), op: "=", values: []string{strings.TrimSpace(v)}}
		case strings.HasPrefix(term, "!"):
			req = v1Requirement{key: strings.TrimSpace(term[1:]), op: "!"}
		case strings.ContainsAny(term, "<>"):
			return nil, bad("only =, ==, !=, in, notin, exists and !key are supported")
		default:
			req = v1Requirement{key: term, op: "exists"}
		}
		if !v1LabelKeyRE.MatchString(req.key) {
			return nil, bad("invalid label key " + strconv.Quote(req.key))
		}
		for _, v := range req.values {
			if !v1LabelValueRE.MatchString(v) {
				return nil, bad("invalid label value " + strconv.Quote(v))
			}
		}
		out = append(out, req)
	}
	return out, nil
}

// v1Fields are the fields a fieldSelector may name.
var v1Fields = map[string]bool{"metadata.name": true, "metadata.namespace": true}

func parseFieldSelector(s string) ([]v1Requirement, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []v1Requirement
	for _, term := range strings.Split(s, ",") {
		op := "="
		k, v, ok := strings.Cut(term, "!=")
		if ok {
			op = "!="
		} else if k, v, ok = strings.Cut(term, "="); ok {
			v = strings.TrimPrefix(v, "=")
		} else {
			return nil, apierror.InvalidArgument("fieldSelector %q: invalid selector: %q; can't understand %q", s, s, term)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !v1Fields[k] {
			return nil, apierror.InvalidArgument("fieldSelector %q: field label not supported: %s", s, k)
		}
		out = append(out, v1Requirement{key: k, op: op, values: []string{v}})
	}
	return out, nil
}

func (q v1Requirement) matches(m map[string]string) bool {
	v, has := m[q.key]
	switch q.op {
	case "exists":
		return has
	case "!":
		return !has
	case "=":
		return has && v == q.values[0]
	case "!=":
		return !has || v != q.values[0]
	case "in":
		return has && contains(q.values, v)
	case "notin":
		return !has || !contains(q.values, v)
	}
	return false
}

func contains(vs []string, v string) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}

// selected reports whether an object's metadata matches the selectors.
func selected(meta *runv1.ObjectMeta, sel v1Selector) bool {
	if meta == nil {
		meta = &runv1.ObjectMeta{}
	}
	for _, q := range sel.labels {
		if !q.matches(meta.Labels) {
			return false
		}
	}
	if len(sel.fields) > 0 {
		f := map[string]string{"metadata.name": meta.Name, "metadata.namespace": meta.Namespace}
		for _, q := range sel.fields {
			if !q.matches(f) {
				return false
			}
		}
	}
	return true
}

// only is the one value a label must have to be selected, when the
// selector pins it (key=value, or key in (value)); "" otherwise. A list
// uses it to read only that job's or execution's children.
func (sel v1Selector) only(key string) string {
	for _, q := range sel.labels {
		if q.key == key && (q.op == "=" || q.op == "in") && len(q.values) == 1 {
			return q.values[0]
		}
	}
	return ""
}
