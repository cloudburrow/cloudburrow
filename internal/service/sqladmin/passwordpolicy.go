package sqladmin

// An instance's password policy (#1876): settings.passwordValidationPolicy,
// as Google documents it for MySQL and PostgreSQL
// (docs.cloud.google.com/sql/docs/mysql/built-in-authentication and
// .../postgres/built-in-authentication). It must be enabled explicitly
// (enablePasswordPolicy); then users.insert and users.update check a
// password against it:
//   - minLength: "Minimum number of characters allowed";
//   - complexity COMPLEXITY_DEFAULT: "A combination of lowercase,
//     uppercase, numeric, and non-alphanumeric characters";
//   - reuseInterval: "Number of previous passwords that cannot be reused",
//     0 to 100, MySQL 8.0 and later and PostgreSQL;
//   - disallowUsernameSubstring: "Disallow username as a part of the
//     password";
//   - passwordChangeInterval: "Minimum interval after which the password
//     can be changed. This flag is only supported for PostgreSQL."
// The previous passwords are kept only as hashes, in the instance's record.
// A password set in SQL (ALTER USER, CREATE ROLE) is not checked: the
// policy is the Admin API's here, not the server's (docs/cloudsql.md).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// passwordPolicy is settings.passwordValidationPolicy.
type passwordPolicy struct {
	MinLength                 int    `json:"minLength"`
	Complexity                string `json:"complexity"`
	ReuseInterval             int    `json:"reuseInterval"`
	DisallowUsernameSubstring bool   `json:"disallowUsernameSubstring"`
	PasswordChangeInterval    string `json:"passwordChangeInterval"`
	EnablePasswordPolicy      bool   `json:"enablePasswordPolicy"`
}

// policyOf is spec's password policy, nil when it has none.
func policyOf(spec Spec) *passwordPolicy {
	raw, ok := spec.Settings["passwordValidationPolicy"]
	if !ok || raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var p passwordPolicy
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

// checkPasswordPolicy refuses a policy Cloud SQL does not take.
func checkPasswordPolicy(spec Spec) error {
	raw, ok := spec.Settings["passwordValidationPolicy"]
	if !ok || raw == nil {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return Errorf(http.StatusBadRequest, "Invalid request: passwordValidationPolicy must be an object.")
	}
	b, _ := json.Marshal(m)
	var p struct {
		MinLength     *json.Number `json:"minLength"`
		ReuseInterval *json.Number `json:"reuseInterval"`
		Complexity    *string      `json:"complexity"`
		ChangeEvery   *string      `json:"passwordChangeInterval"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return Errorf(http.StatusBadRequest, "Invalid request: passwordValidationPolicy: %v", err)
	}
	if p.MinLength != nil {
		if n, err := p.MinLength.Int64(); err != nil || n < 0 {
			return Errorf(http.StatusBadRequest, "Invalid request: passwordValidationPolicy.minLength must be a non-negative integer.")
		}
	}
	if p.ReuseInterval != nil {
		n, err := p.ReuseInterval.Int64()
		if err != nil || n < 0 || n > 100 {
			return Errorf(http.StatusBadRequest, "Invalid request: passwordValidationPolicy.reuseInterval must be between 0 and 100.")
		}
		if n > 0 && spec.DatabaseVersion == "MYSQL_5_7" {
			return Errorf(http.StatusBadRequest, "Invalid request: passwordValidationPolicy.reuseInterval is supported only on "+
				"Cloud SQL for MySQL 8.0 and later.")
		}
	}
	if p.Complexity != nil {
		switch *p.Complexity {
		case "", "COMPLEXITY_UNSPECIFIED", "COMPLEXITY_DEFAULT":
		default:
			return Errorf(http.StatusBadRequest, "Invalid request: Invalid value for passwordValidationPolicy.complexity: %s.", *p.Complexity)
		}
	}
	if p.ChangeEvery != nil && *p.ChangeEvery != "" {
		if spec.MySQL() {
			return Errorf(http.StatusBadRequest, "Invalid request: passwordValidationPolicy.passwordChangeInterval is only supported for PostgreSQL.")
		}
		if _, err := parseProtoDuration(*p.ChangeEvery); err != nil {
			return Errorf(http.StatusBadRequest, "Invalid request: passwordValidationPolicy.passwordChangeInterval %q is not a duration.", *p.ChangeEvery)
		}
	}
	return nil
}

// parseProtoDuration parses a google.protobuf.Duration's JSON form, "3600s".
func parseProtoDuration(s string) (time.Duration, error) {
	if !strings.HasSuffix(s, "s") {
		return 0, &json.UnsupportedValueError{Str: s}
	}
	return time.ParseDuration(s)
}

// mergePasswordPolicy is a patch's passwordValidationPolicy over the one
// the instance has: the fields sent change, the others stay, as gcloud's
// --password-policy-* flags send only the ones given.
func mergePasswordPolicy(cur, next any) any {
	c, ok1 := cur.(map[string]any)
	n, ok2 := next.(map[string]any)
	if !ok1 || !ok2 {
		return next
	}
	out := map[string]any{}
	for k, v := range c {
		out[k] = v
	}
	for k, v := range n {
		out[k] = v
	}
	return out
}

// passwordKey is the user a password belongs to.
// MySQL's user with no host is '%'.
func passwordKey(name, host string) string {
	if host == "" {
		host = "%"
	}
	return name + "@" + host
}

func passwordHash(key, password string) string {
	sum := sha256.Sum256([]byte(key + "\x00" + password))
	return hex.EncodeToString(sum[:])
}

func policyErr(why string) error {
	return Errorf(http.StatusBadRequest, "Invalid request: The password does not meet the instance's password validation policy: %s.", why)
}

// checkPassword checks a password users.insert (change false) or
// users.update (change true) sets against the policy of the created
// instance named instance, if it has one enabled.
func (s *Server) checkPassword(project, instance, name, host, password string, change bool) error {
	c := s.createdNamed(project, instance)
	if c == nil || password == "" {
		return nil
	}
	s.emu.RLock()
	spec := c.spec
	s.emu.RUnlock()
	p := policyOf(spec)
	if p == nil || !p.EnablePasswordPolicy {
		return nil
	}
	if n := len([]rune(password)); p.MinLength > 0 && n < p.MinLength {
		return policyErr("it must be at least " + strconv.Itoa(p.MinLength) + " characters long")
	}
	if p.Complexity == "COMPLEXITY_DEFAULT" {
		var lower, upper, digit, other bool
		for _, r := range password {
			switch {
			case unicode.IsLower(r):
				lower = true
			case unicode.IsUpper(r):
				upper = true
			case unicode.IsDigit(r):
				digit = true
			default:
				other = true
			}
		}
		if !lower || !upper || !digit || !other {
			return policyErr("it must combine lowercase, uppercase, numeric and non-alphanumeric characters")
		}
	}
	if p.DisallowUsernameSubstring && name != "" && strings.Contains(strings.ToLower(password), strings.ToLower(name)) {
		return policyErr("it must not contain the username")
	}
	key := passwordKey(name, host)
	if p.ReuseInterval > 0 {
		h := passwordHash(key, password)
		hist := spec.PasswordHistory[key]
		for i := len(hist) - 1; i >= 0 && i >= len(hist)-p.ReuseInterval; i-- {
			if hist[i] == h {
				return policyErr("it is one of the user's last " + strconv.Itoa(p.ReuseInterval) + " passwords")
			}
		}
	}
	if change && p.PasswordChangeInterval != "" && !spec.MySQL() {
		every, err := parseProtoDuration(p.PasswordChangeInterval)
		if t, terr := time.Parse(time.RFC3339Nano, spec.PasswordChanged[key]); err == nil && terr == nil && every > 0 &&
			time.Since(t) < every {
			return policyErr("the password was changed less than " + p.PasswordChangeInterval + " ago")
		}
	}
	return nil
}

// notePassword records a password users.insert or update set, for the
// policy's reuse and change interval checks.
func (s *Server) notePassword(project, instance, name, host, password string) {
	c := s.createdNamed(project, instance)
	if c == nil || password == "" {
		return
	}
	key := passwordKey(name, host)
	s.emu.Lock()
	defer s.emu.Unlock()
	if c.spec.PasswordHistory == nil {
		c.spec.PasswordHistory = map[string][]string{}
	}
	if c.spec.PasswordChanged == nil {
		c.spec.PasswordChanged = map[string]string{}
	}
	hist := append(c.spec.PasswordHistory[key], passwordHash(key, password))
	if len(hist) > 100 {
		hist = hist[len(hist)-100:]
	}
	c.spec.PasswordHistory[key] = hist
	c.spec.PasswordChanged[key] = time.Now().UTC().Format(time.RFC3339Nano)
	_ = s.saveLocked()
}

// forgetPasswords drops a deleted user's recorded passwords.
func (s *Server) forgetPasswords(project, instance, name, host string) {
	c := s.createdNamed(project, instance)
	if c == nil {
		return
	}
	key := passwordKey(name, host)
	s.emu.Lock()
	defer s.emu.Unlock()
	if _, ok := c.spec.PasswordHistory[key]; !ok {
		return
	}
	delete(c.spec.PasswordHistory, key)
	delete(c.spec.PasswordChanged, key)
	_ = s.saveLocked()
}
