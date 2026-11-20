package run

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"

	dcpb "cloud.google.com/go/developerconnect/apiv1/developerconnectpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Developer Connect's git tokens (#1955): FetchReadToken and
// FetchReadWriteToken on a git repository link, as Google's reference
// documents them. The response carries the token, its expirationTime and
// the gitUsername to clone with. On Google the token is the provider's
// (a GitHub installation token, say); here the provider is the instance's
// own Secure Source Manager, so the token is one its git server
// (securesourcemanagergit.go) accepts, scoped to the link's repository: a
// read token fetches from it (git-upload-pack) and a push with it
// (git-receive-pack) is refused 403; a read-write token does both. Either
// is refused 403 on any other repository, and 401 once expired, after its
// link is deleted, or when its connection is disabled. The token is sent
// as git's HTTP basic password (with any user name) or as a bearer token.
//
// CloudBurrow's choices where Google documents none for Secure Source
// Manager: a token lives an hour (Google says only that expirationTime is
// empty when the expiry is unknown or the token does not expire); the
// gitUsername is "oauth2" (the git server checks only the token); tokens are held in memory and do not outlive
// the instance; a push with a token is the token fetcher's.

const (
	dcTokenTTL      = time.Hour
	dcTokenUsername = "oauth2"
	dcTokenPrefix   = "cbdc."
)

type dcToken struct {
	link, repo string
	write      bool
	account    string
	expires    time.Time
}

var dcTokens = struct {
	mu sync.Mutex
	m  map[string]dcToken
}{m: map[string]dcToken{}}

// mintLinkToken makes a token for a link's repository.
func (d *DeveloperConnectServer) mintLinkToken(ctx context.Context, link string, write bool) (string, time.Time, error) {
	if _, err := parseLinkName(link); err != nil {
		return "", time.Time{}, err
	}
	ssmState.mu.Lock()
	loadDCLocked()
	_, ok := dcState.links[link]
	ssmState.mu.Unlock()
	if !ok {
		return "", time.Time{}, apierror.NotFound("git repository link %q not found", link)
	}
	repo, err := devConnectLinkRepo(link)
	if err != nil {
		return "", time.Time{}, apierror.FailedPrecondition("%v", err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, apierror.Internal(err, "a token")
	}
	kind := "r."
	if write {
		kind = "rw."
	}
	tok := dcTokenPrefix + kind + base64.RawURLEncoding.EncodeToString(raw)
	exp := time.Now().Add(dcTokenTTL).Truncate(time.Second)
	dcTokens.mu.Lock()
	now := time.Now()
	for k, t := range dcTokens.m {
		if now.After(t.expires) {
			delete(dcTokens.m, k)
		}
	}
	dcTokens.m[tok] = dcToken{link: link, repo: repo, write: write, account: ssmCallerOf(ctx), expires: exp}
	dcTokens.mu.Unlock()
	return tok, exp, nil
}

// FetchReadToken mints a token that reads the link's repository.
func (d *DeveloperConnectServer) FetchReadToken(ctx context.Context, req *dcpb.FetchReadTokenRequest) (*dcpb.FetchReadTokenResponse, error) {
	tok, exp, err := d.mintLinkToken(ctx, req.GetGitRepositoryLink(), false)
	if err != nil {
		return nil, err
	}
	return &dcpb.FetchReadTokenResponse{Token: tok, ExpirationTime: timestamppb.New(exp), GitUsername: dcTokenUsername}, nil
}

// FetchReadWriteToken mints a token that reads and pushes to the link's
// repository.
func (d *DeveloperConnectServer) FetchReadWriteToken(ctx context.Context, req *dcpb.FetchReadWriteTokenRequest) (*dcpb.FetchReadWriteTokenResponse, error) {
	tok, exp, err := d.mintLinkToken(ctx, req.GetGitRepositoryLink(), true)
	if err != nil {
		return nil, err
	}
	return &dcpb.FetchReadWriteTokenResponse{Token: tok, ExpirationTime: timestamppb.New(exp), GitUsername: dcTokenUsername}, nil
}

// gitRequestToken is a git request's token: its bearer token, or the
// password of its basic credentials.
func gitRequestToken(r *http.Request) string {
	if k, t, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(k, "bearer") {
		return strings.TrimSpace(t)
	}
	if _, pw, ok := r.BasicAuth(); ok {
		return pw
	}
	return ""
}

// isGitWrite reports whether a git request is part of a push.
func isGitWrite(r *http.Request) bool {
	return strings.HasSuffix(r.URL.Path, "/git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack"
}

// checkLinkToken answers a git request that carries a Developer Connect
// token the token does not allow, and reports whether it did. A request
// with no such token is not checked.
func checkLinkToken(w http.ResponseWriter, r *http.Request, repo string) bool {
	tok := gitRequestToken(r)
	if !strings.HasPrefix(tok, dcTokenPrefix) {
		return false
	}
	dcTokens.mu.Lock()
	t, ok := dcTokens.m[tok]
	dcTokens.mu.Unlock()
	if ok {
		if named, err := devConnectLinkRepo(t.link); err != nil || named != t.repo {
			ok = false
		}
	}
	switch {
	case !ok || time.Now().After(t.expires):
		w.Header().Set("WWW-Authenticate", `Basic realm="Secure Source Manager"`)
		http.Error(w, "the Developer Connect token is invalid or expired", http.StatusUnauthorized)
	case t.repo != repo:
		http.Error(w, "the Developer Connect token is for repository "+t.repo+", not this one", http.StatusForbidden)
	case isGitWrite(r) && !t.write:
		http.Error(w, "the Developer Connect token is a read token (FetchReadToken): it cannot push; use FetchReadWriteToken", http.StatusForbidden)
	default:
		return false
	}
	return true
}

// linkTokenAccount is the account that fetched a Developer Connect token.
func linkTokenAccount(tok string) (string, bool) {
	if !strings.HasPrefix(tok, dcTokenPrefix) {
		return "", false
	}
	dcTokens.mu.Lock()
	defer dcTokens.mu.Unlock()
	t, ok := dcTokens.m[tok]
	return t.account, ok
}
