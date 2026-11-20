package iameval

import (
	"context"
	"encoding/json"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// ActAs is the permission to attach a service account to a resource, so
// that the resource runs as it: "To attach a service account to a
// resource, you need ... the iam.serviceAccounts.actAs permission" on the
// service account ("Attach service accounts to resources",
// https://docs.cloud.google.com/iam/docs/attach-service-accounts, read
// 2026-11-20). roles/iam.serviceAccountUser holds it, as do Owner and
// Editor.
const ActAs = "iam.serviceAccounts.actAs"

// IAM is IAM's table for the service accounts actAs is checked on: a
// service account keeps its own policy (who may act as it), and its
// project's, folders' and organization's apply above it.
var IAM = &Service{
	Name:              "iam.googleapis.com",
	PolicyCollections: []string{"serviceAccounts"},
	Types:             map[string]string{"serviceAccounts": "iam.googleapis.com/ServiceAccount"},
}

// ServiceAccountResource is the resource actAs is checked on for account
// (an email), attached to a resource in project: the account's own
// project, which its email names ("sa@P.iam.gserviceaccount.com",
// "P@appspot.gserviceaccount.com"), or, for an account whose email names
// none (the Compute Engine default, "N-compute@developer.gserviceaccount.com",
// and Google-managed agents), the project it is attached in.
func ServiceAccountResource(project, account string) string {
	_, domain, _ := strings.Cut(account, "@")
	switch {
	case strings.HasSuffix(domain, ".iam.gserviceaccount.com"):
		project = strings.TrimSuffix(domain, ".iam.gserviceaccount.com")
	case domain == "appspot.gserviceaccount.com":
		local, _, _ := strings.Cut(account, "@")
		project = local
	}
	return "projects/" + project + "/serviceAccounts/" + account
}

// AuthorizeActAs checks that the caller of bearer may act as account, an
// email, which a request attaches to a resource in project: nil when it
// holds iam.serviceAccounts.actAs on the account (by the account's own
// policy or its project's and ancestors'), when account is "" (the
// service's default identity, which the docs ask no actAs of here) or when
// enforcement is off; else svc's actAs denial (Service.DenyActAs, or its
// Deny of the permission on the account).
func (e *Enforcer) AuthorizeActAs(ctx context.Context, svc *Service, bearer, project, account string) error {
	if e == nil || e.On == nil || !e.On() || account == "" {
		return nil
	}
	// A field may name the account by its resource name,
	// projects/{p}/serviceAccounts/{email} ("-" for any project).
	if rest, ok := strings.CutPrefix(account, "projects/"); ok {
		if p, email, ok := strings.Cut(rest, "/serviceAccounts/"); ok {
			if p != "-" && !strings.Contains(email, ".iam.gserviceaccount.com") {
				project = p
			}
			account = email
		}
	}
	c := Caller{Owner: true}
	if bearer != "" && e.CallerOf != nil {
		c = e.CallerOf(bearer)
	}
	if c.Invalid {
		return Unauthenticated()
	}
	resource := ServiceAccountResource(project, account)
	if e.holds(ctx, IAM, c, ActAs, resource) {
		return nil
	}
	if svc.DenyActAs != nil {
		return svc.DenyActAs(account, resource)
	}
	return svc.Deny(ActAs, resource)
}

// actAsFields are the service account emails the dotted fields of m
// name, a repeated message's every element's.
func actAsFields(m protoreflect.Message, paths []string) []string {
	var out []string
	for _, p := range paths {
		out = append(out, valuesAt(m, strings.Split(p, "."))...)
	}
	return out
}

// jsonActAs are the service account emails a JSON request's body names at
// paths, which are the gRPC request's field paths: body is the HTTP
// rule's body ("*" for the whole request, else the request field the body
// is), and a JSON field is matched by its lowerCamelCase or proto name.
func jsonActAs(raw []byte, body string, paths []string) []string {
	if body == "" || len(raw) == 0 {
		return nil
	}
	var doc any
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	var out []string
	for _, p := range paths {
		parts := strings.Split(p, ".")
		if body != "*" {
			if parts[0] != body {
				continue
			}
			parts = parts[1:]
		}
		out = append(out, jsonStrings(doc, parts)...)
	}
	return out
}

func jsonStrings(v any, parts []string) []string {
	if len(parts) == 0 {
		switch x := v.(type) {
		case string:
			if x != "" {
				return []string{x}
			}
		case []any:
			var out []string
			for _, el := range x {
				if s, ok := el.(string); ok && s != "" {
					out = append(out, s)
				}
			}
			return out
		}
		return nil
	}
	switch x := v.(type) {
	case []any:
		var out []string
		for _, el := range x {
			out = append(out, jsonStrings(el, parts)...)
		}
		return out
	case map[string]any:
		for _, k := range []string{lowerCamel(parts[0]), parts[0]} {
			if el, ok := x[k]; ok {
				return jsonStrings(el, parts[1:])
			}
		}
	}
	return nil
}

func lowerCamel(s string) string {
	var b strings.Builder
	up := false
	for _, r := range s {
		if r == '_' {
			up = true
			continue
		}
		if up && r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		up = false
		b.WriteRune(r)
	}
	return b.String()
}

// fieldValues are the resources a method's Field names in m: the first
// of its "|"-separated paths that is set, every element of a repeated
// one; [""] for none, which only the owner passes.
func fieldValues(m protoreflect.Message, spec string) []string {
	for _, p := range strings.Split(spec, "|") {
		if v := valuesAt(m, strings.Split(p, ".")); len(v) > 0 {
			return projectIDs(p, v)
		}
	}
	return []string{""}
}

// projectIDs are values of the field path p as resource names: a
// project_id field (Cloud Build's) holds a bare project ID, the resource
// "projects/ID".
func projectIDs(p string, v []string) []string {
	if p != "project_id" {
		return v
	}
	out := make([]string, len(v))
	for i, id := range v {
		out[i] = "projects/" + id
	}
	return out
}

// jsonResources is fieldValues of a JSON request: a path the URL sets,
// else the body's.
func jsonResources(fields map[string]string, raw []byte, body, spec string) []string {
	for _, p := range strings.Split(spec, "|") {
		if v := fields[p]; v != "" {
			return projectIDs(p, []string{v})
		}
		if v := jsonActAs(raw, body, []string{p}); len(v) > 0 {
			return projectIDs(p, v)
		}
	}
	return []string{""}
}

// valuesAt are the strings at parts in m, a repeated string's or a
// repeated message's every one.
func valuesAt(m protoreflect.Message, parts []string) []string {
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(parts[0]))
	if fd == nil {
		return nil
	}
	if len(parts) == 1 {
		if fd.Kind() != protoreflect.StringKind {
			return nil
		}
		if fd.IsList() {
			var out []string
			l := m.Get(fd).List()
			for i := 0; i < l.Len(); i++ {
				if v := l.Get(i).String(); v != "" {
					out = append(out, v)
				}
			}
			return out
		}
		if v := m.Get(fd).String(); v != "" {
			return []string{v}
		}
		return nil
	}
	if fd.Kind() != protoreflect.MessageKind || fd.IsMap() {
		return nil
	}
	if fd.IsList() {
		var out []string
		l := m.Get(fd).List()
		for i := 0; i < l.Len(); i++ {
			out = append(out, valuesAt(l.Get(i).Message(), parts[1:])...)
		}
		return out
	}
	if !m.Has(fd) {
		return nil
	}
	return valuesAt(m.Get(fd).Message(), parts[1:])
}
