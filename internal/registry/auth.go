package registry

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Registry token auth (#2111), as Artifact Registry's Docker registry
// answers: the API's base, /v2/, asks a client with no credentials for a
// bearer token (WWW-Authenticate: Bearer realm="…/v2/token",service="…"),
// which the token endpoint issues for Basic credentials. The credentials
// are those Google documents
// (https://docs.cloud.google.com/artifact-registry/docs/docker/authentication,
// read 2026-11-21): the username oauth2accesstoken and an access token as
// the password; _json_key or _json_key_base64 and a service account key;
// or what a credential helper returns, gcloud's (_dcgcloud_token) or
// docker-credential-gcr's (_dcgcr_…), whose password is an access token
// too. The token issued is the access token itself, or one this instance
// mints for the key's account, so every later request carries a token
// the instance's IAM evaluator names the caller from. Auth.Authorize then
// checks artifactregistry.repositories.downloadArtifacts for a pull and
// uploadArtifacts for a push on the repository.

// Permissions the registry checks on a repository.
const (
	PermissionDownload  = "artifactregistry.repositories.downloadArtifacts"
	PermissionUpload    = "artifactregistry.repositories.uploadArtifacts"
	PermissionDeleteVer = "artifactregistry.versions.delete"
	PermissionDeleteTag = "artifactregistry.tags.delete"
)

// AnonymousToken is the token issued to a client that sends no
// credentials; a request bearing it is one with no credentials.
const AnonymousToken = "anonymous"

// TokenPath is the token endpoint, the realm /v2/ names.
const TokenPath = "/v2/token"

// ErrUnauthenticated is credentials the instance does not accept.
var ErrUnauthenticated = errors.New("authentication failed")

// DeniedError is a caller without a permission on a repository.
type DeniedError struct{ Permission, Resource string }

func (e *DeniedError) Error() string {
	// Google's wording of the Docker registry's refusal.
	return fmt.Sprintf("Permission %q denied on resource %q (or it may not exist).", e.Permission, e.Resource)
}

// Auth authenticates and authorizes registry requests.
type Auth struct {
	// Login turns a username and password into a bearer token:
	// ErrUnauthenticated for credentials it does not accept.
	Login func(username, password string) (token string, err error)
	// Authorize checks that the caller of bearer ("" for a request with
	// no credentials) holds permission on the repository resource
	// (projects/P/locations/L/repositories/R): nil, ErrUnauthenticated
	// or a *DeniedError.
	Authorize func(bearer, permission, resource string) error
	// Required reports whether a request with no credentials is asked
	// for them (--iam-enforcement=on), as Google's registry asks for a
	// pull of a repository that is not public. A client then fetches
	// a token, which with no credentials is AnonymousToken.
	Required func() bool
}

// accessTokenUser reports whether a username's password is an access
// token: oauth2accesstoken, or a credential helper's.
func accessTokenUser(u string) bool {
	return u == "oauth2accesstoken" || u == "_token" || u == "_dcgcloud_token" || strings.HasPrefix(u, "_dcgcr_")
}

// KeyCredential is the service account key JSON a username names in its
// password: _json_key's as it is, _json_key_base64's decoded; ok is false
// for any other username.
func KeyCredential(username, password string) (key []byte, ok bool) {
	switch username {
	case "_json_key":
		return []byte(password), true
	case "_json_key_base64":
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(password))
		if err != nil {
			return []byte{}, true
		}
		return b, true
	}
	return nil, false
}

// IsAccessTokenUser reports whether username's password is an access token.
func IsAccessTokenUser(username string) bool { return accessTokenUser(username) }

// realm is the token endpoint's URL and the service, from the host the
// client reached.
func realm(r *http.Request) (string, string) {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host + TokenPath, r.Host
}

// challenge answers 401 with the Bearer challenge; scope, when set, is
// the access the request needed.
func (h *Handler) challenge(w http.ResponseWriter, r *http.Request, scope, msg string) {
	rl, svc := realm(r)
	v := fmt.Sprintf("Bearer realm=%q,service=%q", rl, svc)
	if scope != "" {
		v += fmt.Sprintf(",scope=%q", scope)
	}
	w.Header().Set("WWW-Authenticate", v)
	h.fail(w, http.StatusUnauthorized, "UNAUTHORIZED", msg)
}

// credentials are a request's: its bearer token, or the token its Basic
// credentials log in to. present is false when it sends none.
func (h *Handler) credentials(r *http.Request) (bearer string, present bool, err error) {
	v := r.Header.Get("Authorization")
	if v == "" {
		return "", false, nil
	}
	if tok, ok := cutScheme(v, "Bearer"); ok {
		if tok == AnonymousToken {
			return "", true, nil
		}
		return tok, true, nil
	}
	if u, p, ok := r.BasicAuth(); ok {
		tok, err := h.login(u, p)
		if tok == AnonymousToken {
			tok = ""
		}
		return tok, true, err
	}
	return "", true, ErrUnauthenticated
}

func cutScheme(v, scheme string) (string, bool) {
	if len(v) > len(scheme)+1 && strings.EqualFold(v[:len(scheme)], scheme) && v[len(scheme)] == ' ' {
		return strings.TrimSpace(v[len(scheme)+1:]), true
	}
	return "", false
}

func (h *Handler) login(u, p string) (string, error) {
	if h.Auth == nil || h.Auth.Login == nil {
		return AnonymousToken, nil
	}
	return h.Auth.Login(u, p)
}

// ping answers /v2/: 200 with credentials the instance accepts, else the
// challenge.
func (h *Handler) ping(w http.ResponseWriter, r *http.Request) bool {
	if h.Auth == nil {
		return true
	}
	bearer, present, err := h.credentials(r)
	if !present {
		h.challenge(w, r, "", "authentication required")
		return false
	}
	if err == nil && bearer != "" && h.Auth.Authorize != nil {
		// A bearer token the instance did not mint.
		err = h.Auth.Authorize(bearer, "", "")
	}
	if errors.Is(err, ErrUnauthenticated) {
		h.challenge(w, r, "", "authentication failed: the credentials are not valid")
		return false
	}
	return true
}

// token serves the token endpoint: GET with Basic credentials (docker,
// crane), or the OAuth2 form POST containerd sends.
func (h *Handler) token(w http.ResponseWriter, r *http.Request) {
	var u, p string
	var have bool
	switch r.Method {
	case http.MethodGet:
		u, p, have = r.BasicAuth()
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			h.fail(w, http.StatusBadRequest, "UNSUPPORTED", "invalid form")
			return
		}
		switch r.PostForm.Get("grant_type") {
		case "password":
			u, p, have = r.PostForm.Get("username"), r.PostForm.Get("password"), true
		case "refresh_token":
			// The token issued is its own refresh token.
			u, p, have = "oauth2accesstoken", r.PostForm.Get("refresh_token"), true
		default:
			u, p, have = r.BasicAuth()
		}
	default:
		h.fail(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
		return
	}
	tok := AnonymousToken
	if have {
		var err error
		if tok, err = h.login(u, p); err != nil {
			h.fail(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication failed: the credentials are not valid")
			return
		}
	}
	now := time.Now().UTC()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "access_token": tok, "expires_in": 3600,
		"issued_at": now.Format(time.RFC3339)})
}

// authorize checks a request's access to repo; it answers the refusal and
// returns false, or returns true.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, repo Repo, name, permission string) bool {
	if h.Auth == nil || h.Auth.Authorize == nil {
		return true
	}
	action := "pull"
	if permission != PermissionDownload {
		action = "push,pull"
	}
	scope := "repository:" + name + ":" + action
	bearer, present, err := h.credentials(r)
	if !present && h.Auth.Required != nil && h.Auth.Required() {
		h.challenge(w, r, scope, "authentication required")
		return false
	}
	if err == nil {
		err = h.Auth.Authorize(bearer, permission, RepoResource(repo))
	}
	var de *DeniedError
	switch {
	case err == nil:
		return true
	case errors.As(err, &de):
		h.fail(w, http.StatusForbidden, "DENIED", de.Error())
	case errors.Is(err, ErrUnauthenticated):
		h.challenge(w, r, scope, "authentication failed: the credentials are not valid")
	default:
		h.fail(w, http.StatusForbidden, "DENIED", err.Error())
	}
	return false
}

// RepoResource is a repository's resource name.
func RepoResource(r Repo) string {
	return "projects/" + r.Project + "/locations/" + r.Location + "/repositories/" + r.Repository
}

// pushPermission is a request's permission on its repository.
func pushPermission(r *http.Request) string {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return PermissionDownload
	}
	return PermissionUpload
}
