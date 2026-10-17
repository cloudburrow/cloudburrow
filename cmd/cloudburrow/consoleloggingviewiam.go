package main

// A log view's IAM policy (#1519) on the Log storage screen: each view's
// row, _AllLogs' too, has Permissions, which shows the view's role
// bindings and replaces them through the same IAMPolicy service
// logging_v2.yaml binds on views. Stored, not enforced (ADR-0006).

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const viewPermissions = "view-permissions"

// viewBindingsText renders a policy's bindings, one role per line followed
// by its principals.
func viewBindingsText(pol *iampb.Policy) string {
	var lines []string
	for _, b := range pol.GetBindings() {
		members := append([]string(nil), b.GetMembers()...)
		sort.Strings(members)
		lines = append(lines, b.GetRole()+" "+strings.Join(members, ", "))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// parseViewBindings reads the Permissions form's bindings.
func parseViewBindings(text string) ([]*iampb.Binding, error) {
	var out []*iampb.Binding
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		role, rest, ok := strings.Cut(line, " ")
		if !ok || !strings.HasPrefix(role, "roles/") {
			return nil, fmt.Errorf("%q is not a role followed by its principals, such as roles/logging.viewAccessor user:a@example.com", line)
		}
		b := &iampb.Binding{Role: role}
		for _, m := range strings.FieldsFunc(rest, func(r rune) bool { return r == ',' || r == ' ' }) {
			b.Members = append(b.Members, m)
		}
		if len(b.Members) == 0 {
			return nil, fmt.Errorf("%s names no principal", role)
		}
		out = append(out, b)
	}
	return out, nil
}

// viewPermissionsAction is a view's Permissions form, filled with its policy.
func (p loggingBucketsProvider) viewPermissionsAction(ctx context.Context, view string) console.Action {
	def := ""
	if a, err := p.admin(); err == nil {
		if pol, err := a.ViewIAM().GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: view}); err == nil {
			def = viewBindingsText(pol)
		}
	}
	return console.Action{ID: viewPermissions, Label: "Permissions", Fields: []console.Field{{
		Name: "bindings", Label: "Role bindings", Type: "textarea", Default: def,
		Help: "One role per line, then its principals, comma-separated: roles/logging.viewAccessor user:a@example.com, " +
			"group:g@example.com. This replaces the view's policy; clear it to remove every binding. " +
			"Stored and returned as set, never enforced (ADR-0006)."}}}
}

// setViewPermissions replaces a view's bindings, under its current etag.
func setViewPermissions(ctx context.Context, api iampb.IAMPolicyServer, view, text string) error {
	bindings, err := parseViewBindings(text)
	if err != nil {
		return err
	}
	cur, err := api.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: view})
	if err != nil {
		return err
	}
	_, err = api.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: view,
		Policy: &iampb.Policy{Bindings: bindings, Etag: cur.GetEtag()}})
	return err
}
