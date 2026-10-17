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
// fieldSelector is ignored. services.list documents it as "Not supported,
// and ignored by Cloud Run", and every other v1 list as "Not supported by
// Cloud Run" (docs read 2026-10-17:
// https://docs.cloud.google.com/run/docs/reference/rest/v1/namespaces.services/list,
// https://docs.cloud.google.com/run/docs/reference/rest/v1/namespaces.executions/list),
// so a list returns what its labelSelector alone selects, whatever the
// fieldSelector says. gcloud beta sends metadata.name={service} to
// projects.locations.services.list to find a service's region, and matches
// the name again itself.

// v1Requirement is one term of a selector.
type v1Requirement struct {
	key string
	// op is "=", "!=", "in", "notin", "exists" or "!".
	op     string
	values []string
}

// v1Selector is a list call's label selector.
type v1Selector struct {
	labels []v1Requirement
}

var (
	// A label key: an optional DNS-subdomain prefix and a name, as
	// Kubernetes allows; a value: a name or empty.
	v1LabelKeyRE   = regexp.MustCompile(`^([a-z0-9]([-a-z0-9.]*[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
	v1LabelValueRE = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$`)
)

// labelSelector parses a list request's labelSelector; its fieldSelector
// is ignored, as Cloud Run documents.
func labelSelector(r *http.Request) (v1Selector, error) {
	labels, err := parseLabelSelector(r.URL.Query().Get("labelSelector"))
	if err != nil {
		return v1Selector{}, err
	}
	return v1Selector{labels: labels}, nil
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
