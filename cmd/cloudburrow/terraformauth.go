package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
)

// How `cloudburrow terraform` signs the provider in.
//
// Without IAM enforcement every call is served, and the provider presents
// terraformAccessToken. Under --iam-enforcement=on a served API answers a
// bearer token this instance did not mint with 401 UNAUTHENTICATED, as
// Google does, so "owner" cannot be sent: the provider signs in as a
// principal, with the credentials the user names, the way hashicorp/google
// reads them, in its order:
//
//  1. GOOGLE_OAUTH_ACCESS_TOKEN: an access token, sent as access_token
//     (one this instance minted, such as gcloud auth print-access-token
//     with the local configuration).
//  2. GOOGLE_CREDENTIALS, GOOGLE_CLOUD_KEYFILE_JSON, GCLOUD_KEYFILE_JSON,
//     then GOOGLE_APPLICATION_CREDENTIALS: a service account key file (a
//     path or its JSON), sent as credentials. The provider signs a JWT with
//     the key and exchanges it at the file's token_uri for an access token
//     (the JWT-bearer grant; the provider asks for no self-signed JWT in
//     the default universe), so the token_uri must be this instance's token
//     endpoint, as every key the IAM API here creates has.
//  3. None: the instance's own credentials fixture (the account `env`
//     exports), which holds every permission, as the project's owner.
//
// A file that would have the provider reach Google is refused before
// Terraform runs: a key whose token_uri is another host, an authorized_user
// file (its refresh goes to oauth2.googleapis.com), an external_account or
// impersonated_service_account file, and impersonate_service_account (the
// provider calls iamcredentials.googleapis.com for it, at a URL no endpoint
// argument changes).

// terraformAuth is what the provider block authenticates with: access_token
// when credentials is empty.
type terraformAuth struct {
	accessToken string
	credentials string
}

// terraformOwnerAuth is the provider's sign-in without enforcement.
var terraformOwnerAuth = terraformAuth{accessToken: terraformAccessToken}

// terraformCredentialVars are the provider's credentials variables, in its
// order of precedence.
var terraformCredentialVars = []string{"GOOGLE_CREDENTIALS", "GOOGLE_CLOUD_KEYFILE_JSON", "GCLOUD_KEYFILE_JSON", "GOOGLE_APPLICATION_CREDENTIALS"}

// impersonationArg finds impersonate_service_account set in a module.
var impersonationArg = regexp.MustCompile(`(?m)^\s*impersonate_service_account\s*=`)

// terraformEnforcedAuth is the provider's sign-in on an instance enforcing
// IAM, from getenv and the module in dir, and a cleanup that removes any
// file it wrote.
func terraformEnforcedAuth(cfg config.Config, dir string, getenv func(string) string) (terraformAuth, func(), error) {
	none := func() {}
	if getenv("GOOGLE_IMPERSONATE_SERVICE_ACCOUNT") != "" || moduleImpersonates(dir) {
		return terraformAuth{}, none, fmt.Errorf("refusing to run: impersonate_service_account makes the provider call " +
			"iamcredentials.googleapis.com, which no endpoint argument points here; sign in as the service account " +
			"with a key file (GOOGLE_CREDENTIALS) or an access token (GOOGLE_OAUTH_ACCESS_TOKEN) instead")
	}
	if tok := strings.TrimSpace(getenv("GOOGLE_OAUTH_ACCESS_TOKEN")); tok != "" {
		return terraformAuth{accessToken: tok}, none, nil
	}
	for _, v := range terraformCredentialVars {
		val := strings.TrimSpace(getenv(v))
		if val == "" {
			continue
		}
		body := []byte(val)
		isPath := !strings.HasPrefix(val, "{")
		if isPath {
			b, err := os.ReadFile(val)
			if err != nil {
				return terraformAuth{}, none, fmt.Errorf("%s: %w", v, err)
			}
			body = b
		}
		if err := checkLocalKeyFile(cfg, body); err != nil {
			return terraformAuth{}, none, fmt.Errorf("refusing to run: %s %w", v, err)
		}
		if isPath {
			abs, err := filepath.Abs(val)
			if err != nil {
				return terraformAuth{}, none, err
			}
			return terraformAuth{credentials: abs}, none, nil
		}
		// The JSON itself: written to a file of the run's own, so the
		// provider block names a path rather than the key.
		f, err := os.CreateTemp(cfg.InstanceDir(), "terraform-credentials-*.json")
		if err != nil {
			return terraformAuth{}, none, err
		}
		_, werr := f.Write(body)
		cerr := f.Close()
		path := f.Name()
		cleanup := func() { _ = os.Remove(path) }
		if werr != nil || cerr != nil {
			cleanup()
			return terraformAuth{}, none, fmt.Errorf("write credentials file: %v %v", werr, cerr)
		}
		return terraformAuth{credentials: path}, cleanup, nil
	}
	// The instance's own fixture, as `env` writes it.
	creds, err := metadata.LoadOrCreate(cfg.InstanceDir(), cfg.DefaultProject(), tokenURI(cfg))
	if err != nil {
		return terraformAuth{}, none, err
	}
	path, err := creds.WriteADC(cfg.InstanceDir())
	if err != nil {
		return terraformAuth{}, none, err
	}
	return terraformAuth{credentials: path}, none, nil
}

// moduleImpersonates reports whether a .tf file in dir sets
// impersonate_service_account.
func moduleImpersonates(dir string) bool {
	files, _ := filepath.Glob(filepath.Join(dir, "*.tf"))
	for _, f := range files {
		if strings.HasPrefix(filepath.Base(f), "cloudburrow_providers") {
			continue
		}
		if b, err := os.ReadFile(f); err == nil && impersonationArg.Match(b) {
			return true
		}
	}
	return false
}

// checkLocalKeyFile accepts a service account key file whose token_uri is
// this instance's token endpoint (a loopback or bind address at its
// metadata port), and refuses anything that would sign in at Google.
func checkLocalKeyFile(cfg config.Config, body []byte) error {
	var f struct {
		Type     string `json:"type"`
		TokenURI string `json:"token_uri"`
	}
	if err := json.Unmarshal(body, &f); err != nil {
		return fmt.Errorf("is not a credentials file: %v", err)
	}
	if f.Type != "service_account" {
		return fmt.Errorf("is a %q credentials file; under IAM enforcement the provider signs in here with a "+
			"service account key this instance issued, or GOOGLE_OAUTH_ACCESS_TOKEN", f.Type)
	}
	u, err := url.Parse(f.TokenURI)
	if err != nil || u.Host == "" {
		return fmt.Errorf("has no usable token_uri")
	}
	host, port := u.Hostname(), u.Port()
	local := host == "localhost" || host == cfg.BindAddress
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		local = true
	}
	if !local || port != strconv.Itoa(cfg.Endpoints.Metadata) {
		return fmt.Errorf("names token_uri %s, not this instance's token endpoint (%s): the provider would sign in there", f.TokenURI, tokenURI(cfg))
	}
	return nil
}
