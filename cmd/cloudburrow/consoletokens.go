package main

// The Auth header fields of Create task and Create job (#1243): an OIDC or
// OAuth token, minted locally at each attempt, as the API now accepts.

import (
	"fmt"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Auth header choices, as Google's console names them.
const (
	authNone  = "None"
	authOIDC  = "Add OIDC token"
	authOAuth = "Add OAuth token"
)

// authTokenFields are the token fields, in section, for an HTTP target.
func authTokenFields(section string) []console.Field {
	return []console.Field{
		{Name: "authHeader", Label: "Auth header", Type: "select", Section: section, Default: authNone,
			Options: []string{authNone, authOIDC, authOAuth},
			Help: "An OIDC token (for Cloud Run and other services that check an ID token) or an OAuth " +
				"token (for Google APIs), sent as Authorization: Bearer. Minted locally and signed with " +
				"this instance's key: it does not verify against Google's certificates."},
		{Name: "serviceAccountEmail", Label: "Service account", Type: "text", Section: section,
			Help: "Required with a token: the account the token is for, such as invoker@my-project.iam.gserviceaccount.com."},
		{Name: "audience", Label: "Audience", Type: "text", Section: section,
			Help: "Optional, OIDC only. Empty is the target URL."},
		{Name: "scope", Label: "Scope", Type: "text", Section: section,
			Help: "Optional, OAuth only. Empty is https://www.googleapis.com/auth/cloud-platform."},
	}
}

// authTokenFieldsFor is authTokenFields prefilled with a stored token, for
// an edit form, so saving it keeps the token.
func authTokenFieldsFor(section string, t authToken) []console.Field {
	fields := authTokenFields(section)
	switch t.kind {
	case "oidc":
		fields[0].Default = authOIDC
	case "oauth":
		fields[0].Default = authOAuth
	}
	fields[1].Default, fields[2].Default, fields[3].Default = t.email, t.audience, t.scope
	return fields
}

// authToken is what the form asks for. kind is "", "oidc" or "oauth".
type authToken struct{ kind, email, audience, scope string }

// parseAuthToken reads the token fields. A field that does not belong to the
// chosen token is refused rather than dropped.
func parseAuthToken(values map[string]string) (authToken, error) {
	t := authToken{
		email:    strings.TrimSpace(values["serviceAccountEmail"]),
		audience: strings.TrimSpace(values["audience"]),
		scope:    strings.TrimSpace(values["scope"]),
	}
	switch choice := strings.TrimSpace(values["authHeader"]); choice {
	case "", authNone:
		if t.email != "" || t.audience != "" || t.scope != "" {
			return authToken{}, fmt.Errorf("a service account, audience or scope needs an auth header: choose %q or %q", authOIDC, authOAuth)
		}
		return authToken{}, nil
	case authOIDC:
		if t.scope != "" {
			return authToken{}, fmt.Errorf("scope is for an OAuth token; an OIDC token takes an audience")
		}
		t.kind = "oidc"
	case authOAuth:
		if t.audience != "" {
			return authToken{}, fmt.Errorf("audience is for an OIDC token; an OAuth token takes a scope")
		}
		t.kind = "oauth"
	default:
		return authToken{}, fmt.Errorf("auth header %q is not one of %s, %s or %s", choice, authNone, authOIDC, authOAuth)
	}
	if t.email == "" {
		return authToken{}, fmt.Errorf("a token needs its service account")
	}
	return t, nil
}

// authTokenProperties shows a stored token on a detail page.
func authTokenProperties(kind, email, audienceOrScope, url string) []console.Property {
	switch kind {
	case "oidc":
		aud := audienceOrScope
		if aud == "" {
			aud = url + " (the URL)"
		}
		return []console.Property{{Label: "Auth header", Value: "OIDC token"},
			{Label: "Service account", Value: email}, {Label: "Audience", Value: aud}}
	case "oauth":
		scope := audienceOrScope
		if scope == "" {
			scope = "https://www.googleapis.com/auth/cloud-platform (the default)"
		}
		return []console.Property{{Label: "Auth header", Value: "OAuth token"},
			{Label: "Service account", Value: email}, {Label: "Scope", Value: scope}}
	}
	return nil
}
