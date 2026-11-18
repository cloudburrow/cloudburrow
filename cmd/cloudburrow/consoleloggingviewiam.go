package main

// A log view's IAM policy (#1519) on the Log storage screen: each view's
// row, _AllLogs' too, has Permissions, which shows the view's role
// bindings and replaces them through the same IAMPolicy service
// logging_v2.yaml binds on views. Stored, not enforced (ADR-0006).
//
// A view's page also has the Permissions tab every other service's
// resource has (#1748): it reads the policy at version 3 and grants and
// edits IAM Conditions, which Cloud Logging stores on a view. The row's
// text form edits the bindings without a condition and keeps the
// conditional ones as they are.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const viewPermissions = "view-permissions"

// viewBindingsText renders a policy's bindings without a condition, one
// role per line followed by its principals.
func viewBindingsText(pol *iampb.Policy) string {
	var lines []string
	for _, b := range pol.GetBindings() {
		if b.GetCondition() != nil {
			continue
		}
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
		if pol, err := a.ViewIAM().GetIamPolicy(ctx, viewPolicyRequest(view)); err == nil {
			def = viewBindingsText(pol)
		}
	}
	return console.Action{ID: viewPermissions, Label: "Permissions", Fields: []console.Field{{
		Name: "bindings", Label: "Role bindings", Type: "textarea", Default: def,
		Help: "One role per line, then its principals, comma-separated: roles/logging.viewAccessor user:a@example.com, " +
			"group:g@example.com. This replaces the view's bindings without a condition; clear it to remove them all. " +
			"Conditional bindings are kept: grant and edit them on the view's Permissions tab. " +
			"Stored and returned as set, never enforced (ADR-0006)."}}}
}

// viewPolicyRequest reads a view's policy at version 3, so a conditional
// binding comes with its condition rather than as ROLE_withcond_HASH.
func viewPolicyRequest(view string) *iampb.GetIamPolicyRequest {
	return &iampb.GetIamPolicyRequest{Resource: view, Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}}
}

// setViewPermissions replaces a view's bindings, under its current etag.
func setViewPermissions(ctx context.Context, api iampb.IAMPolicyServer, view, text string) error {
	bindings, err := parseViewBindings(text)
	if err != nil {
		return err
	}
	cur, err := api.GetIamPolicy(ctx, viewPolicyRequest(view))
	if err != nil {
		return err
	}
	pol := &iampb.Policy{Version: 1, Bindings: bindings, Etag: cur.GetEtag()}
	for _, b := range cur.GetBindings() {
		if b.GetCondition() != nil {
			pol.Bindings = append(pol.Bindings, b)
			pol.Version = 3
		}
	}
	_, err = api.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: view, Policy: pol})
	return err
}

// PolicyOn is the Permissions tab on a view's page, _AllLogs' too; a
// bucket has no policy of its own.
func (loggingBucketsProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 2 {
		return nil
	}
	return &console.PolicyTarget{Link: compatibilityDoc + "cloud-logging--googleloggingv2",
		RoleHelp: roleHelp("Cloud Logging", "roles/logging.viewAccessor"), Conditions: true}
}

// withViewIAM calls fn with the view IAMPolicy service and the view's name,
// once the bucket is the project's.
func (p loggingBucketsProvider) withViewIAM(ctx context.Context, project string, path []string, fn func(iamServer, string) error) error {
	if len(path) != 2 {
		return errors.New("no log view named")
	}
	if err := bucketOfParent(ctx, project, path[0]); err != nil {
		return err
	}
	a, err := p.admin()
	if err != nil {
		return err
	}
	return fn(a.ViewIAM(), path[0]+"/views/"+path[1])
}

func (p loggingBucketsProvider) GetPolicy(ctx context.Context, project string, path []string) (pol console.Policy, err error) {
	err = p.withViewIAM(ctx, project, path, func(api iamServer, name string) error {
		pol, err = getProtoPolicy(ctx, api, name)
		return err
	})
	return pol, err
}

func (p loggingBucketsProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	return p.withViewIAM(ctx, project, path, func(api iamServer, name string) error {
		return setProtoPolicy(ctx, api, name, pol)
	})
}

func (p loggingBucketsProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) (held []string, err error) {
	err = p.withViewIAM(ctx, project, path, func(api iamServer, name string) error {
		held, err = testProtoPermissions(ctx, api, name, perms)
		return err
	})
	return held, err
}

var _ console.PolicyEditor = loggingBucketsProvider{}
