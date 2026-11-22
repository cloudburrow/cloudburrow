package main

// Run identities (#2173): Colab Enterprise notebook executions and
// schedules, Vertex AI pipeline jobs and Dataform workflow invocations run
// as the service account (or user) they name. Attaching an account needs
// iam.serviceAccounts.actAs on it, checked by the IAM enforcer when it is
// on; each run's calls carry a token this instance's credentials mint for
// the account, as IAM Credentials' generateAccessToken does, so with
// enforcement on they are allowed or refused as that account's.

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/oauth2"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/service/dataform"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// runIdentity is the hooks a service's runs take their identities from:
// actAs on svc, the caller's email from its bearer token, and a token
// minted for an account (a service account must exist and be enabled).
func runIdentity(e *iameval.Enforcer, svc *iameval.Service, creds *metadata.Credentials, reg func() *resourcemanager.Registry) vertexai.Identity {
	return vertexai.Identity{
		ActAs: func(ctx context.Context, project, account string) error {
			if e == nil {
				return nil
			}
			return e.AuthorizeActAs(ctx, svc, iameval.IncomingBearer(ctx), project, account)
		},
		Caller: func(ctx context.Context) string { return callerEmail(e, ctx) },
		Mint:   func(account string) (string, error) { return mintRunToken(creds, reg, account) },
	}
}

// callerEmail is the email the caller's bearer token was minted for, ""
// for the instance's own credentials or when enforcement is off.
func callerEmail(e *iameval.Enforcer, ctx context.Context) string {
	if e == nil || e.On == nil || !e.On() || e.CallerOf == nil {
		return ""
	}
	tok := iameval.IncomingBearer(ctx)
	if tok == "" {
		return ""
	}
	c := e.CallerOf(tok)
	if c.Owner || c.Invalid {
		return ""
	}
	return c.Email
}

// mintRunToken is a token for account: a service account that exists and
// is enabled (as IAM Credentials mints for), or a user.
func mintRunToken(creds *metadata.Credentials, reg func() *resourcemanager.Registry, account string) (string, error) {
	if creds == nil {
		return "", nil
	}
	email := strings.TrimPrefix(strings.TrimPrefix(account, "serviceAccount:"), "user:")
	if strings.HasSuffix(email, ".gserviceaccount.com") && reg != nil {
		if r := reg(); r != nil {
			a, ok := r.ServiceAccountByName(email)
			switch {
			case !ok && strings.HasSuffix(email, ".iam.gserviceaccount.com"):
				return "", fmt.Errorf("Service account %s does not exist", email)
			case ok && a.Disabled:
				return "", fmt.Errorf("Service account %s is disabled", email)
			case ok:
				email = a.Email
			}
		}
	}
	return creds.MintAccessTokenFor(email, metadata.TokenTTL), nil
}

// dataformRunAuth is a Dataform invocation's credentials (#2173): its
// service account's token, else none (the instance's own).
func dataformRunAuth(ctx context.Context) option.ClientOption {
	if tok := dataform.RunToken(ctx); tok != "" {
		return option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok, TokenType: "Bearer"}))
	}
	return option.WithoutAuthentication()
}
