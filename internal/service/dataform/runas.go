package dataform

// Strict act-as (#2173), as "Use strict act-as mode"
// (https://docs.cloud.google.com/dataform/docs/strict-act-as-mode, read
// 2026-11-22) documents it: "Creating or updating a repository",
// "Creating or updating a workflow configuration", "Creating a workflow
// invocation" and "Updating a release configuration" need
// iam.serviceAccounts.actAs (the Service Account User role's) on the
// effective service account. An invocation's is its workflow config's
// when it invokes one; otherwise "the service account listed in the
// WorkflowInvocation.InvocationConfig field is used if it's set", and "this
// defaults to the service account configured at the repository level".
//
// The invocation then runs as that account: its BigQuery jobs, notebook
// runs and Cloud Storage writes carry a token this instance mints for it,
// so with IAM enforcement on they are allowed or refused as that
// account's. With no account named it runs with the instance's own
// credentials, as before.
//
// UNVERIFIED (re-read 2026-11-22): a release config names no service
// account, so its update checks the repository's; the check is made
// whether or not a project opted into strict mode (an instance has no
// opt-in setting), and an invocation naming no account is not refused.

import (
	"context"
	"strings"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
)

type runTokenKey struct{}

// RunToken is the bearer token of the invocation ctx runs, "" for the
// instance's own credentials.
func RunToken(ctx context.Context) string {
	f, _ := ctx.Value(runTokenKey{}).(func() string)
	if f == nil {
		return ""
	}
	return f()
}

// projectOf is the project of a Dataform resource name.
func projectOf(name string) string {
	rest, _ := strings.CutPrefix(name, "projects/")
	p, _, _ := strings.Cut(rest, "/")
	return p
}

// actAs checks the caller may act as account, attached in name's project.
func (s *Server) actAs(ctx context.Context, name, account string) error {
	if s.opts.ActAs == nil || account == "" {
		return nil
	}
	return s.opts.ActAs(ctx, projectOf(name), account)
}

// effectiveAccount is an invocation's account: its config's, else its
// repository's.
func effectiveAccount(cfg *dfpb.InvocationConfig, repo *dfpb.Repository) string {
	if sa := cfg.GetServiceAccount(); sa != "" {
		return sa
	}
	return repo.GetServiceAccount()
}

// withRunAs is ctx carrying account's token for the invocation's calls;
// the error is the account's (one that does not exist or is disabled).
func (s *Server) withRunAs(ctx context.Context, account string) (context.Context, error) {
	if s.opts.RunAs == nil || account == "" {
		return ctx, nil
	}
	tok, err := s.opts.RunAs(account)
	if err != nil {
		return ctx, err
	}
	if tok == "" {
		return ctx, nil
	}
	return context.WithValue(ctx, runTokenKey{}, func() string { return tok }), nil
}
