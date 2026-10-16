package main

// The /identity probe (#1388): what the metadata server tells this
// workload it runs as, read through the official libraries the way an
// application on Cloud Run reads it.
//
//   - cloud.google.com/go/compute/metadata's Email("default"), at the
//     injected GCE_METADATA_HOST;
//   - an identity token from google.golang.org/api/idtoken, whose email
//     claim names the account;
//   - an access token from Application Default Credentials, which only
//     has to be one CloudBurrow minted.
//
// `envprobe identity` prints the same line and exits, for a job's task.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/idtoken"
)

func identityProbe(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	host, err := injected("GCE_METADATA_HOST")
	if err != nil {
		return "", err
	}
	email, err := metadata.EmailWithContext(ctx, "default")
	if err != nil {
		return "", fmt.Errorf("metadata Email: %w", err)
	}
	ts, err := idtoken.NewTokenSource(ctx, "https://identity-probe.example")
	if err != nil {
		return "", fmt.Errorf("idtoken.NewTokenSource: %w", err)
	}
	tok, err := ts.Token()
	if err != nil {
		return "", fmt.Errorf("an identity token: %w", err)
	}
	parts := strings.Split(tok.AccessToken, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("the identity token is not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode the identity token: %w", err)
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", fmt.Errorf("decode the identity token's claims: %w", err)
	}
	creds, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return "", fmt.Errorf("google.FindDefaultCredentials: %w", err)
	}
	at, err := creds.TokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("an access token: %w", err)
	}
	if !strings.HasPrefix(at.AccessToken, "cbl_") {
		return "", fmt.Errorf("the access token was not minted by CloudBurrow")
	}
	return fmt.Sprintf("email=%s id_token_email=%s access_token=local metadata=%s", email, claims.Email, host), nil
}
