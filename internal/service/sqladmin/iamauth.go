package sqladmin

// IAM database authentication (#1944, ADR-0004's amendment of 2026-11-19),
// for PostgreSQL and MySQL, as Google documents it
// (https://docs.cloud.google.com/sql/docs/postgres/iam-authentication,
// .../mysql/iam-authentication, .../postgres/add-manage-iam-users,
// .../mysql/add-manage-iam-users, .../mysql/iam-logins, read 2026-11-19):
//
//   - users.insert of type CLOUD_IAM_USER or CLOUD_IAM_SERVICE_ACCOUNT
//     creates a database role named as Google names it: on PostgreSQL the
//     user's email in lower case, and a service account's email without
//     ".gserviceaccount.com" (sa@project.iam), at most 63 characters; on
//     MySQL the part before the @, at most 32 characters, with the full
//     email answered as iamEmail. The role has no privileges, as Google's
//     has none "on any databases, by default".
//   - The instance's cloudsql.iam_authentication flag (PostgreSQL) or
//     cloudsql_iam_authentication (MySQL) must be on for such a role to
//     sign in. It is Cloud SQL's flag, not the server's, so it is kept in
//     the instance's settings and never passed to the server.
//   - A sign-in needs an OAuth access token that this instance minted and
//     that is still live, checked by its SHA-256 against the instance's
//     log of the tokens it minted (internal/metadata's issued.go), as
//     Memorystore's IAM auth (#1614) checks one; never a token from
//     Google. The token's account must be the role's principal, and hold
//     cloudsql.instances.login on the instance (roles/cloudsql.instanceUser
//     grants it), evaluated by the instance's IAM policy evaluator.
//
// The sign-in is checked by the connector listener (iamwire.go), which is
// where the Cloud SQL connectors and the Auth Proxy arrive: the token is
// either the one generateEphemeralCert embedded in the client certificate
// (WithIAMAuthN, --auto-iam-authn), or the password the client sends
// (manual IAM sign-in: PostgreSQL's cleartext password, MySQL's
// mysql_clear_password plugin). The listener then signs in to the server
// as the role on the client's behalf with the role's own password, which
// is derived from a key kept owner-only in the state directory and is
// never shown to anyone. The server holds the role to that password:
// PostgreSQL's pg_hba.conf requires one (md5) for members of
// cloudsqliamuser and cloudsqliamserviceaccount, ahead of the trust line
// every other user has; MySQL checks every account's password. A client
// that reaches the server's own port and names an IAM role is therefore
// refused, whatever it sends.
//
// Not served: CLOUD_IAM_GROUP and its members' automatic accounts, and a
// token sent as a password on the server's own port rather than through
// the connector listener.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// IAM user types, as the Admin API names them.
const (
	TypeBuiltIn           = "BUILT_IN"
	TypeIAMUser           = "CLOUD_IAM_USER"
	TypeIAMServiceAccount = "CLOUD_IAM_SERVICE_ACCOUNT"
	TypeIAMGroup          = "CLOUD_IAM_GROUP"
)

// LoginPermission is the permission an IAM sign-in needs on the instance.
const LoginPermission = "cloudsql.instances.login"

// ExecuteSQLPermission is the permission executeSql needs.
const ExecuteSQLPermission = "cloudsql.instances.executeSql"

// IAMSource is what IAM database authentication checks a token against.
type IAMSource struct {
	// Token is the live access token this instance minted whose SHA-256
	// (hex) is hash: the account it was minted for and when it expires.
	Token func(hash string, now time.Time) (email string, expires time.Time, ok bool)
	// Allowed reports whether email holds permission on the instance.
	Allowed func(project, instance, email, permission string) bool
}

// iamAuth is the server's IAM database authentication state.
type iamAuth struct {
	mu  sync.Mutex
	src *IAMSource
	key []byte
}

// UseIAM makes IAM database sign-ins check tokens against src. Without it
// every IAM sign-in is refused.
func (s *Server) UseIAM(src IAMSource) {
	s.iam.mu.Lock()
	s.iam.src = &src
	s.iam.mu.Unlock()
}

func (s *Server) iamSource() *IAMSource {
	s.iam.mu.Lock()
	defer s.iam.mu.Unlock()
	return s.iam.src
}

// iamKeyFile is the key IAM roles' passwords are derived from.
const iamKeyFile = "cloudsql-iam-role-key"

// roleKey is the key IAM roles' passwords are derived from: read from the
// state directory, made there on first use (in memory when there is none).
func (s *Server) roleKey() []byte {
	s.iam.mu.Lock()
	defer s.iam.mu.Unlock()
	if s.iam.key != nil {
		return s.iam.key
	}
	dir := ""
	if s.conn != nil {
		dir = s.conn.dir
	}
	if dir != "" {
		path := filepath.Join(dir, iamKeyFile)
		if b, err := os.ReadFile(path); err == nil && len(b) == 64 {
			if k, err := hex.DecodeString(string(b)); err == nil {
				s.iam.key = k
				return k
			}
		}
		k := make([]byte, 32)
		_, _ = rand.Read(k)
		if os.WriteFile(path, []byte(hex.EncodeToString(k)), 0o600) == nil {
			s.iam.key = k
			return k
		}
	}
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	s.iam.key = k
	return k
}

// rolePassword is the password of the IAM role named role: the same for
// every instance, so a replica's copy of the role signs in as its
// primary's does.
func (s *Server) rolePassword(role string) string {
	m := hmac.New(sha256.New, s.roleKey())
	m.Write([]byte("cloudsql-iam-role\x00" + role))
	return hex.EncodeToString(m.Sum(nil))
}

// iamEmailRE is the email an IAM user or service account may have.
var iamEmailRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._%+-]*@[a-z0-9][a-z0-9.-]*\.[a-z]{2,}$`)

const saSuffix = ".gserviceaccount.com"

// iamDBName is the database user Google names for email of type typ on
// an engine (mysql or not), or an error naming what is wrong.
func iamDBName(typ, email string, mysql bool) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !iamEmailRE.MatchString(email) {
		return "", Errorf(http.StatusBadRequest, "Invalid request: %s is not the email of an IAM principal.", email)
	}
	sa := strings.HasSuffix(email, saSuffix)
	switch typ {
	case TypeIAMServiceAccount:
		if !sa {
			return "", Errorf(http.StatusBadRequest, "Invalid request: %s is not a service account's email; a CLOUD_IAM_SERVICE_ACCOUNT user is named by one.", email)
		}
	case TypeIAMUser:
		if sa {
			return "", Errorf(http.StatusBadRequest, "Invalid request: %s is a service account; create it as type CLOUD_IAM_SERVICE_ACCOUNT.", email)
		}
	}
	name := email
	limit := 63
	if mysql {
		name, _, _ = strings.Cut(email, "@")
		limit = 32
	} else if sa {
		name = strings.TrimSuffix(email, saSuffix)
	}
	if len(name) > limit {
		return "", Errorf(http.StatusBadRequest, "Invalid request: the database user name %s is longer than %d characters.", name, limit)
	}
	return name, nil
}

// iamFlag is the name of the instance flag that turns IAM sign-ins on.
func iamFlag(mysql bool) string {
	if mysql {
		return "cloudsql_iam_authentication"
	}
	return "cloudsql.iam_authentication"
}

// isIAMFlag is whether name is Cloud SQL's IAM authentication flag.
func isIAMFlag(name string) bool {
	return name == "cloudsql.iam_authentication" || name == "cloudsql_iam_authentication"
}

// IsCloudSQLOnlyFlag is whether a flag is Cloud SQL's own, kept in the
// instance's settings and not passed to the server.
func IsCloudSQLOnlyFlag(name string) bool { return isIAMFlag(name) }

// iamFlagOn is whether the instance's IAM authentication flag is on.
func (s *Server) iamFlagOn(project string, e Engine) bool {
	c := s.createdNamed(project, e.Instance())
	if c == nil {
		return false
	}
	s.emu.RLock()
	defer s.emu.RUnlock()
	for _, f := range c.spec.Flags {
		if f.Name == iamFlag(isMySQLEngine(e)) && strings.EqualFold(f.Value, "on") {
			return true
		}
	}
	return false
}

func isMySQLEngine(e Engine) bool { return strings.HasPrefix(e.DatabaseVersion(), "MYSQL") }

// iamUserEngine is an engine that keeps IAM users.
type iamUserEngine interface {
	// CreateIAMUser creates u (its Type an IAM type) for email, signing
	// in with password only.
	CreateIAMUser(ctx context.Context, u User, email, password string) error
}

// iamLogin is the outcome of checking an IAM sign-in: the role to sign in
// as and its password, or why it is refused.
type iamLogin struct {
	role, password string
}

// errIAMRefused is a refused IAM sign-in; its message is the server's.
type errIAMRefused struct{ msg string }

func (e errIAMRefused) Error() string { return e.msg }

// iamUserNamed is the IAM user of e named name, or ok false when name is
// not one.
func iamUserNamed(ctx context.Context, e Engine, name string) (User, bool) {
	us, err := e.ListUsers(ctx)
	if err != nil {
		return User{}, false
	}
	for _, u := range us {
		if u.Name == name && (u.Type == TypeIAMUser || u.Type == TypeIAMServiceAccount) {
			return u, true
		}
	}
	return User{}, false
}

// checkIAMLogin checks a sign-in to project's instance e as the IAM user
// u with the access token whose SHA-256 is hash.
func (s *Server) checkIAMLogin(project string, e Engine, u User, hash string) (iamLogin, error) {
	refused := func(why string) (iamLogin, error) {
		kind := "user"
		if u.Type == TypeIAMServiceAccount {
			kind = "service account"
		}
		return iamLogin{}, errIAMRefused{msg: "Cloud SQL IAM " + kind + " authentication failed for user \"" + u.Name + "\": " + why}
	}
	if !s.iamFlagOn(project, e) {
		return refused("the instance's " + iamFlag(isMySQLEngine(e)) + " flag is not on")
	}
	src := s.iamSource()
	if src == nil || src.Token == nil || src.Allowed == nil {
		return refused("this instance checks no access tokens")
	}
	email, _, ok := src.Token(hash, time.Now())
	if !ok || email == "" {
		return refused("the access token is not one this instance issued, or it has expired")
	}
	want, err := iamDBName(u.Type, email, isMySQLEngine(e))
	if err != nil || want != u.Name {
		return refused("the access token is " + email + "'s, not this user's")
	}
	if !src.Allowed(project, e.Instance(), strings.ToLower(email), LoginPermission) {
		return refused(email + " does not have " + LoginPermission + " (roles/cloudsql.instanceUser) on the instance")
	}
	return iamLogin{role: u.Name, password: s.rolePassword(u.Name)}, nil
}

// tokenHash is a token's SHA-256 in hex, as the minted tokens' log keeps it.
func tokenHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// tokenURNPrefix is how generateEphemeralCert embeds an access token in the
// client certificate: a URI subject alternative name holding the token's
// SHA-256, which the connector listener reads back. The token itself is
// not written, as the hash is all a check needs.
const tokenURNPrefix = "urn:cloudburrow:cloudsql:iam-token-sha256:"

// insertIAMUser is users.insert of an IAM user or service account: the
// role Google names for the email in name, with a password of its own
// that only the connector listener uses.
func (s *Server) insertIAMUser(ctx context.Context, base, project string, e Engine, typ, email, host, password string) (any, error) {
	ie, ok := e.(iamUserEngine)
	if !ok {
		return nil, unimplemented("users of type "+typ+" on "+e.DatabaseVersion(), "IAM database authentication is served for PostgreSQL and MySQL")
	}
	if password != "" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: a %s user has no password; it signs in with an IAM access token.", typ)
	}
	if host != "" && host != "%" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: a %s user has no host.", typ)
	}
	name, err := iamDBName(typ, email, isMySQLEngine(e))
	if err != nil {
		return nil, err
	}
	u := User{Name: name, Type: typ, IAMEmail: strings.ToLower(strings.TrimSpace(email))}
	if isMySQLEngine(e) {
		u.Host = "%"
	}
	if err := ie.CreateIAMUser(ctx, u, u.IAMEmail, s.rolePassword(name)); err != nil {
		return nil, err
	}
	return s.done(base, project, e, "CREATE_USER", name), nil
}
