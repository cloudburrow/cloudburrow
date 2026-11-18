package metadata

import (
	"crypto/rsa"
	"net/http"
	"strings"
	"time"
)

// Service accounts (#1747). With a directory set (the project registry's
// IAM service accounts, through cmd/cloudburrow), IAM Credentials acts as
// Google's does rather than for any name (docs.cloud.google.com/iam/docs/
// service-account-creds, .../service-account-impersonation, read
// 2026-11-17):
//
//   - the caller is the account a live access token this instance minted
//     names; any other bearer, or none, is 401 UNAUTHENTICATED;
//   - the target must be a live account the caller holds the verb's
//     permission on (iam.serviceAccounts.getAccessToken, getOpenIdToken,
//     signBlob or signJwt), on the account or its project, and every
//     delegate in a chain iam.serviceAccounts.implicitDelegation on the
//     next; otherwise 403 "Permission 'P' denied on resource (or it may
//     not exist).";
//   - a service account may not mint an access token for, or sign as,
//     itself: FAILED_PRECONDITION;
//   - a disabled account is refused: FAILED_PRECONDITION;
//   - an access token lasts at most 3,600 seconds (no
//     constraints/iam.allowServiceAccountCredentialLifetimeExtension is
//     modelled);
//   - signBlob and signJwt sign with the account's system-managed key.
//
// The token endpoint accepts an assertion signed with a user-managed key of
// a live, enabled account (a key file CreateServiceAccountKey returned),
// and refuses one whose key is disabled, deleted or of a disabled or
// deleted account, as Google's does ("invalid_grant").
//
// The instance's own account (the ADC fixture's) is not in the directory;
// impersonating it, by name or as "-", keeps the earlier behaviour.

// ServiceAccountDirectory is what the metadata server needs of the IAM
// service accounts.
type ServiceAccountDirectory interface {
	// Account resolves an account name (an email or a unique ID); ok is
	// false when it is not a live account.
	Account(name string) (acct DirectoryAccount, ok bool)
	// Granted reports whether member (serviceAccount:E or user:E) holds
	// permission on the account named, by its policy or its project's.
	Granted(member, account, permission string) bool
	// Signer is the account's system-managed key and its ID.
	Signer(account string) (*rsa.PrivateKey, string, error)
	// Key is the public key a key ID names: known is false when no account
	// has it, usable false when it or its account is disabled or deleted.
	Key(keyID string, now time.Time) (pub *rsa.PublicKey, email string, known, usable bool)
	// PublicCerts are a live account's enabled, valid keys, by email or
	// unique ID; ok is false when it is not a live account.
	PublicCerts(account string) (certs []PublicCert, ok bool)
}

// DirectoryAccount is one live account.
type DirectoryAccount struct {
	Email    string
	UniqueID string
	Disabled bool
}

// SetServiceAccounts sets the directory; nil keeps the earlier behaviour.
func (c *Credentials) SetServiceAccounts(d ServiceAccountDirectory) {
	c.dirMu.Lock()
	c.dir = d
	c.dirMu.Unlock()
}

func (c *Credentials) directory() ServiceAccountDirectory {
	c.dirMu.Lock()
	defer c.dirMu.Unlock()
	return c.dir
}

// tokenAccount is the account a live access token was minted for.
func (c *Credentials) tokenAccount(tok string, now time.Time) (string, bool) {
	h := TokenHash(tok)
	for _, t := range c.LiveTokens(now) {
		if t.Hash == h {
			return t.Email, t.Email != ""
		}
	}
	return "", false
}

// member is an email's IAM principal.
func member(email string) string {
	if strings.HasSuffix(email, ".gserviceaccount.com") {
		return "serviceAccount:" + email
	}
	return "user:" + email
}

// verbPermission is the permission each IAM Credentials method needs.
var verbPermission = map[string]string{
	"generateAccessToken": "iam.serviceAccounts.getAccessToken",
	"generateIdToken":     "iam.serviceAccounts.getOpenIdToken",
	"signBlob":            "iam.serviceAccounts.signBlob",
	"signJwt":             "iam.serviceAccounts.signJwt",
}

// iamDenied is Google's answer to a caller without the permission, or for
// an account that does not exist.
func iamDenied(w http.ResponseWriter, perm string) {
	iamError(w, http.StatusForbidden, "PERMISSION_DENIED", "Permission '"+perm+"' denied on resource (or it may not exist).")
}

// authorize checks a call to verb on account with delegates; it writes the
// refusal and returns ok false, or returns the target account.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, d ServiceAccountDirectory, verb, account string, delegates []string) (DirectoryAccount, bool) {
	perm, known := verbPermission[verb]
	if !known {
		iamError(w, http.StatusNotFound, "NOT_FOUND", "unknown IAM Credentials method "+verb)
		return DirectoryAccount{}, false
	}
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	caller, live := "", false
	if ok {
		caller, live = s.creds.tokenAccount(strings.TrimSpace(bearer), time.Now())
	}
	if !live {
		iamError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "Request had invalid authentication credentials. Expected OAuth 2 access token, login cookie or other valid authentication credential.")
		return DirectoryAccount{}, false
	}
	target, ok := d.Account(account)
	if !ok {
		iamDenied(w, perm)
		return DirectoryAccount{}, false
	}
	if len(delegates) == 0 && strings.EqualFold(caller, target.Email) && verb != "generateIdToken" {
		iamError(w, http.StatusBadRequest, "FAILED_PRECONDITION", "You can't create a token for the same service account that you used to authenticate the request.")
		return DirectoryAccount{}, false
	}
	// The chain: caller -> delegates... -> target, each hop granted on the
	// next.
	hops := append([]string{}, delegates...)
	from := member(caller)
	for _, del := range hops {
		name := strings.TrimPrefix(del, "projects/-/serviceAccounts/")
		next, ok := d.Account(name)
		if !ok || !d.Granted(from, next.Email, "iam.serviceAccounts.implicitDelegation") {
			iamDenied(w, "iam.serviceAccounts.implicitDelegation")
			return DirectoryAccount{}, false
		}
		from = member(next.Email)
	}
	if !d.Granted(from, target.Email, perm) {
		iamDenied(w, perm)
		return DirectoryAccount{}, false
	}
	if target.Disabled {
		iamError(w, http.StatusBadRequest, "FAILED_PRECONDITION", "The service account "+target.Email+" is disabled.")
		return DirectoryAccount{}, false
	}
	return target, true
}

// TokenAccount is the account a live access token this instance minted
// names: how the IAM API's deprecated SignBlob and SignJwt (#1751) know
// their caller, as IAM Credentials does.
func (c *Credentials) TokenAccount(tok string, now time.Time) (string, bool) {
	return c.tokenAccount(tok, now)
}

// Member is an email's IAM principal: serviceAccount: for a service
// account, user: for anyone else.
func Member(email string) string { return member(email) }
