package vertexai

// Execution identities (#2173): a Colab Enterprise notebook execution, a
// schedule's runs and a Vertex AI pipeline job run as the identity they
// name, as Google documents it:
//
//   - "Colab Enterprise uses this service account's credentials to run
//     your notebook"; with end-user credentials "the runtime uses the
//     permissions associated with your user credentials"
//     (https://docs.cloud.google.com/colab/docs/schedule-notebook-run,
//     read 2026-11-22);
//   - attaching a service account needs iam.serviceAccounts.actAs on it
//     ("Attach service accounts to resources",
//     https://docs.cloud.google.com/iam/docs/attach-service-accounts), the
//     Service Account User role's; for a schedule run as a service account
//     "only users with the iam.serviceAccounts.actAs permission on the
//     service account" can modify or trigger it, and one run with personal
//     credentials "only the specified user";
//   - a pipeline job's service_account: "If not specified, the Compute
//     Engine default service account in the project will be used"
//     (PipelineJob.service_account; actAs on it, as for every attached
//     account).
//
// The create methods' actAs checks are the enforcer's (round8ai.go's
// ActAs rows); this file's hooks check a stored schedule's account on
// update, pause and resume, and give each run its identity's token, so
// with IAM enforcement on its Cloud Storage, Dataform and BigQuery calls
// are allowed or refused as that identity's.
//
// UNVERIFIED (re-read 2026-11-22, schedule-notebook-run): which methods
// "modify" a schedule (update, pause and resume are checked here; delete
// is the schedule's own aiplatform.schedules.delete alone); that an
// execution with execution_user runs with its creator's credentials (the
// caller, who is that user in Google's console), and a schedule's runs with
// those of the user who made it.

import (
	"context"
	"strings"
)

// Identity is how runs take their identities: nil hooks leave every run
// with the instance's own (owner) credentials and check nothing.
type Identity struct {
	// ActAs checks the caller of ctx may act as account in project
	// (iam.serviceAccounts.actAs); nil when it may or enforcement is off.
	ActAs func(ctx context.Context, project, account string) error
	// Caller is the email of the caller of ctx, "" for the instance's own
	// credentials (no bearer token).
	Caller func(ctx context.Context) string
	// Mint is a fresh access token for account (a service account, which
	// must exist and be enabled, or a user).
	Mint func(account string) (string, error)
}

// SetIdentity installs the execution identity hooks.
func (s *Server) SetIdentity(id Identity) {
	s.mu.Lock()
	s.identity = id
	s.mu.Unlock()
}

func (s *Server) ident() Identity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identity
}

// callerOf is the caller's email, "" for the owner.
func (s *Server) callerOf(ctx context.Context) string {
	if id := s.ident(); id.Caller != nil {
		return id.Caller(ctx)
	}
	return ""
}

// checkActAs is the caller's actAs on account, attached in project.
func (s *Server) checkActAs(ctx context.Context, project, account string) error {
	if id := s.ident(); id.ActAs != nil && account != "" {
		return id.ActAs(ctx, project, account)
	}
	return nil
}

// runAsToken is a token for account, "" when there is none to run as
// (account "" or no Mint hook).
func (s *Server) runAsToken(account string) (string, error) {
	id := s.ident()
	if id.Mint == nil || account == "" {
		return "", nil
	}
	return id.Mint(account)
}

type runAsKey struct{}

// WithRunAsToken is ctx carrying the bearer token a run's calls use.
func WithRunAsToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, runAsKey{}, token)
}

// RunAsToken is the bearer token of the run ctx belongs to, "" for the
// instance's own credentials.
func RunAsToken(ctx context.Context) string {
	t, _ := ctx.Value(runAsKey{}).(string)
	return t
}

// jobIdentity is the account an execution runs as: its service account,
// or for execution_user the creator (creator, "" for the owner).
func jobIdentity(serviceAccount, executionUser, creator string) string {
	if serviceAccount != "" {
		return strings.TrimPrefix(serviceAccount, "serviceAccount:")
	}
	if executionUser != "" {
		return creator
	}
	return ""
}
