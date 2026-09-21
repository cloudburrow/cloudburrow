// Package secrets implements Google Secret Manager v1.
//
// Like Cloud Tasks, this is an owned implementation: the upstream audit (#24)
// found no official Secret Manager emulator. Everything here is measured
// against the published google.cloud.secretmanager.v1 contract, never against
// another emulator's behaviour.
//
// **It is not a secret store.** CloudBurrow authenticates nothing, so anything
// written here is readable by any caller that can reach the endpoint. It
// exists so an application whose code fetches configuration from Secret
// Manager can run locally, not to protect anything. See docs/credentials.md.
package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/identity-wael/cloudburrow/internal/apierror"
	"github.com/identity-wael/cloudburrow/internal/resource"
	"github.com/identity-wael/cloudburrow/internal/store"
)

// VersionState mirrors google.cloud.secretmanager.v1.SecretVersion.State.
type VersionState string

const (
	// StateEnabled means the version can be accessed.
	StateEnabled VersionState = "ENABLED"
	// StateDisabled means the version exists but cannot be accessed.
	StateDisabled VersionState = "DISABLED"
	// StateDestroyed means the payload is gone. This is terminal: a destroyed
	// version can never be enabled again, and its payload is not recoverable.
	StateDestroyed VersionState = "DESTROYED"
)

// LatestAlias is the version alias the contract defines.
//
// It resolves to "the most recently created SecretVersion" — the contract's
// own words, from GetSecretVersionRequest.Name. Deliberately *not* "the most
// recently enabled one": accessing a disabled latest must fail, because a
// caller silently receiving an older payload than they asked for is worse
// than an error.
const LatestAlias = "latest"

// Secret is a Secret Manager secret.
type Secret struct {
	Name        string            `json:"name"`
	Created     time.Time         `json:"created"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	// Replication records what the caller asked for. Only "automatic" is
	// meaningful locally; a user-managed request is accepted and recorded
	// rather than silently rewritten, so a caller reading it back sees what
	// they set.
	Replication string `json:"replication"`
	// NextVersion is the number the next AddSecretVersion will use. It never
	// decreases, so a destroyed version's number is never reused — reuse
	// would let a stale reference resolve to different bytes.
	NextVersion int    `json:"nextVersion"`
	Etag        string `json:"etag"`
}

// Version is one version of a secret.
type Version struct {
	Name      string       `json:"name"`
	Number    int          `json:"number"`
	State     VersionState `json:"state"`
	Created   time.Time    `json:"created"`
	Destroyed time.Time    `json:"destroyed,omitempty"`
	// Payload is the stored bytes. It is cleared on destroy rather than
	// marked, so a destroyed version cannot leak what it held.
	Payload []byte `json:"payload,omitempty"`
	Etag    string `json:"etag"`
}

// Accessible reports whether this version's payload can be read.
func (v Version) Accessible() bool { return v.State == StateEnabled }

// Store holds secrets and their versions.
type Store struct {
	mu  sync.Mutex
	db  store.Store
	now func() time.Time
}

// NewStore returns a store backed by db.
func NewStore(db store.Store) *Store {
	return &Store{db: db, now: time.Now}
}

// NewStoreWithClock returns a store with an injected clock, so tests can
// assert on creation and destruction times without sleeping.
func NewStoreWithClock(db store.Store, now func() time.Time) *Store {
	return &Store{db: db, now: now}
}

const (
	secretPrefix  = "secrets/"
	versionPrefix = "versions/"
)

// --- name handling -----------------------------------------------------

// ParseSecretName validates projects/{project}/secrets/{secret}.
//
// It parses directly rather than through internal/resource, because Secret
// Manager secrets are **global**: the name has four segments, not the six a
// regional resource has. Passing it through the regional parser would reject
// every valid secret name. The traversal checks that parser applies are
// applied here too, via resource.ValidID.
func ParseSecretName(name string) (project, secret string, err error) {
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "secrets" {
		return "", "", apierror.InvalidArgument(
			"%q is not a secret name; want projects/{project}/secrets/{secret}", name)
	}
	project, secret = parts[1], parts[3]
	if !resource.ValidID(project) {
		return "", "", apierror.InvalidArgument("project %q is not a valid resource ID", project)
	}
	if !resource.ValidID(secret) {
		return "", "", apierror.InvalidArgument("secret %q is not a valid resource ID", secret)
	}
	if err := ValidateSecretID(secret); err != nil {
		return "", "", err
	}
	return project, secret, nil
}

// ParseVersionName validates projects/{project}/secrets/{secret}/versions/{v}
// and returns the version as written, which may be the "latest" alias.
func ParseVersionName(name string) (project, secret, version string, err error) {
	idx := strings.Index(name, "/"+versionPrefix)
	if idx < 0 {
		return "", "", "", apierror.InvalidArgument("%q is not a secret version name", name)
	}
	parent, version := name[:idx], name[idx+len("/"+versionPrefix):]
	if version == "" || strings.Contains(version, "/") {
		return "", "", "", apierror.InvalidArgument("%q is not a secret version name", name)
	}
	project, secret, err = ParseSecretName(parent)
	if err != nil {
		return "", "", "", err
	}
	if version != LatestAlias {
		if n, convErr := strconv.Atoi(version); convErr != nil || n < 1 {
			return "", "", "", apierror.InvalidArgument(
				"secret version must be a positive integer or %q, got %q", LatestAlias, version)
		}
	}
	return project, secret, version, nil
}

// ValidateSecretID enforces the documented ID rules.
//
// Secret Manager allows letters, digits, hyphens and underscores, up to 255
// characters. Rejecting here rather than at the storage layer means a caller
// gets InvalidArgument with the rule, not a backend error.
func ValidateSecretID(id string) error {
	if id == "" {
		return apierror.InvalidArgument("secret ID must not be empty")
	}
	if len(id) > 255 {
		return apierror.InvalidArgument("secret ID must be at most 255 characters, got %d", len(id))
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return apierror.InvalidArgument(
				"secret ID %q contains %q; only letters, digits, hyphens and underscores are allowed", id, r)
		}
	}
	return nil
}

// SecretName renders a secret's resource name.
func SecretName(project, secret string) string {
	return fmt.Sprintf("projects/%s/%s%s", project, secretPrefix, secret)
}

// VersionName renders a version's resource name.
func VersionName(project, secret string, number int) string {
	return fmt.Sprintf("%s/%s%d", SecretName(project, secret), versionPrefix, number)
}

// KubernetesSecretName maps a GCP secret to the name of the Kubernetes Secret
// that will hold it.
//
// The two namespaces do not have the same rules: Secret Manager allows upper
// case and underscores, Kubernetes allows neither. A lossy lowercasing alone
// would collide — `My_Secret` and `my-secret` would land on the same object —
// so a hash of the original name is appended. The hash is of the full
// project-qualified name, so two projects never share an object either.
func KubernetesSecretName(project, secret string) string {
	sum := sha256.Sum256([]byte(project + "/" + secret))
	digest := hex.EncodeToString(sum[:5])

	var b strings.Builder
	for _, r := range strings.ToLower(secret) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	sanitised := strings.Trim(b.String(), "-")
	if sanitised == "" {
		sanitised = "secret"
	}
	// Kubernetes names are at most 253 characters; the prefix, digest and
	// separators take 15, so the readable part is capped well below that.
	const maxReadable = 200
	if len(sanitised) > maxReadable {
		sanitised = sanitised[:maxReadable]
	}
	return "cb-secret-" + sanitised + "-" + digest
}

// VersionKey is the key a version's payload is stored under inside the
// Kubernetes Secret's data map.
func VersionKey(number int) string { return fmt.Sprintf("v%d", number) }

// --- storage -----------------------------------------------------------

func secretKey(project, secret string) string {
	return "secret/" + project + "/" + secret
}

func versionKey(project, secret string, number int) string {
	return fmt.Sprintf("version/%s/%s/%010d", project, secret, number)
}

func (s *Store) etag() string {
	// Etags only have to change on write and be opaque. A counter would be
	// guessable across restarts; the clock plus a hash is neither.
	sum := sha256.Sum256([]byte(strconv.FormatInt(s.now().UnixNano(), 10)))
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// CreateSecret creates a secret.
func (s *Store) CreateSecret(project, id string, labels, annotations map[string]string, replication string) (Secret, error) {
	if project == "" {
		return Secret{}, apierror.InvalidArgument("project must not be empty")
	}
	if err := ValidateSecretID(id); err != nil {
		return Secret{}, err
	}
	if replication == "" {
		replication = "automatic"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := secretKey(project, id)
	if _, err := s.db.Get(key); err == nil {
		return Secret{}, apierror.AlreadyExists("secret %s already exists", SecretName(project, id))
	}

	sec := Secret{
		Name:        SecretName(project, id),
		Created:     s.now().UTC(),
		Labels:      labels,
		Annotations: annotations,
		Replication: replication,
		NextVersion: 1,
		Etag:        s.etag(),
	}
	if err := s.put(key, sec); err != nil {
		return Secret{}, err
	}
	return sec, nil
}

// GetSecret returns a secret.
func (s *Store) GetSecret(project, id string) (Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getSecret(project, id)
}

func (s *Store) getSecret(project, id string) (Secret, error) {
	raw, err := s.db.Get(secretKey(project, id))
	if err != nil {
		return Secret{}, apierror.NotFound("secret %s not found", SecretName(project, id))
	}
	var sec Secret
	if err := json.Unmarshal(raw, &sec); err != nil {
		return Secret{}, apierror.Internal(err, "decode secret %s", SecretName(project, id))
	}
	return sec, nil
}

// UpdateSecret replaces the labels and annotations a caller may change.
//
// Only the mutable fields are touched: name, creation time and version
// numbering are not caller-settable, and accepting them silently would let a
// client believe it had renamed a secret.
func (s *Store) UpdateSecret(project, id string, labels, annotations map[string]string, updateLabels, updateAnnotations bool) (Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sec, err := s.getSecret(project, id)
	if err != nil {
		return Secret{}, err
	}
	if updateLabels {
		sec.Labels = labels
	}
	if updateAnnotations {
		sec.Annotations = annotations
	}
	sec.Etag = s.etag()
	if err := s.put(secretKey(project, id), sec); err != nil {
		return Secret{}, err
	}
	return sec, nil
}

// ListSecrets returns a project's secrets, ordered by name.
func (s *Store) ListSecrets(project string) ([]Secret, error) {
	if project == "" {
		return nil, apierror.InvalidArgument("project must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	keys, err := s.db.List("secret/" + project + "/")
	if err != nil {
		return nil, apierror.Internal(err, "list secrets")
	}
	out := make([]Secret, 0, len(keys))
	for _, k := range keys {
		raw, err := s.db.Get(k)
		if err != nil {
			continue
		}
		var sec Secret
		if err := json.Unmarshal(raw, &sec); err != nil {
			continue
		}
		out = append(out, sec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteSecret removes a secret and every version of it.
//
// Versions are removed with the secret rather than left behind: an orphaned
// version whose secret is gone could be listed by a later secret with the
// same ID, handing a caller another secret's bytes.
func (s *Store) DeleteSecret(project, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.getSecret(project, id); err != nil {
		return err
	}
	keys, err := s.db.List(fmt.Sprintf("version/%s/%s/", project, id))
	if err != nil {
		return apierror.Internal(err, "list versions")
	}
	for _, k := range keys {
		if err := s.db.Delete(k); err != nil {
			return apierror.Internal(err, "delete version")
		}
	}
	if err := s.db.Delete(secretKey(project, id)); err != nil {
		return apierror.Internal(err, "delete secret")
	}
	return nil
}

// MaxPayloadBytes is the documented Secret Manager payload limit.
const MaxPayloadBytes = 64 * 1024

// AddVersion stores a new payload and returns the version created.
func (s *Store) AddVersion(project, id string, payload []byte) (Version, error) {
	if len(payload) == 0 {
		return Version{}, apierror.InvalidArgument("secret payload must not be empty")
	}
	if len(payload) > MaxPayloadBytes {
		return Version{}, apierror.InvalidArgument(
			"secret payload is %d bytes; the limit is %d", len(payload), MaxPayloadBytes)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sec, err := s.getSecret(project, id)
	if err != nil {
		return Version{}, err
	}
	number := sec.NextVersion
	sec.NextVersion++
	sec.Etag = s.etag()
	if err := s.put(secretKey(project, id), sec); err != nil {
		return Version{}, err
	}

	v := Version{
		Name:    VersionName(project, id, number),
		Number:  number,
		State:   StateEnabled,
		Created: s.now().UTC(),
		Payload: append([]byte(nil), payload...),
		Etag:    s.etag(),
	}
	if err := s.put(versionKey(project, id, number), v); err != nil {
		return Version{}, err
	}
	return v, nil
}

// ResolveVersion turns a version reference, which may be "latest", into a
// concrete version number.
func (s *Store) ResolveVersion(project, id, version string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolveVersion(project, id, version)
}

func (s *Store) resolveVersion(project, id, version string) (int, error) {
	if _, err := s.getSecret(project, id); err != nil {
		return 0, err
	}
	if version != LatestAlias {
		n, err := strconv.Atoi(version)
		if err != nil || n < 1 {
			return 0, apierror.InvalidArgument("invalid secret version %q", version)
		}
		return n, nil
	}

	versions, err := s.listVersions(project, id)
	if err != nil {
		return 0, err
	}
	if len(versions) == 0 {
		return 0, apierror.NotFound("secret %s has no versions", SecretName(project, id))
	}
	// The contract defines latest as the most recently created version, not
	// the most recently enabled one. Skipping a disabled latest would hand a
	// caller older bytes than they asked for without saying so.
	best := versions[0]
	for _, v := range versions {
		if v.Number > best.Number {
			best = v
		}
	}
	return best.Number, nil
}

// GetVersion returns a version's metadata. The payload is included; callers
// that must not see it use AccessVersion, which enforces the state rules.
func (s *Store) GetVersion(project, id string, version string) (Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	number, err := s.resolveVersion(project, id, version)
	if err != nil {
		return Version{}, err
	}
	return s.getVersion(project, id, number)
}

func (s *Store) getVersion(project, id string, number int) (Version, error) {
	raw, err := s.db.Get(versionKey(project, id, number))
	if err != nil {
		return Version{}, apierror.NotFound("secret version %s not found", VersionName(project, id, number))
	}
	var v Version
	if err := json.Unmarshal(raw, &v); err != nil {
		return Version{}, apierror.Internal(err, "decode secret version")
	}
	return v, nil
}

// AccessVersion returns a version's payload, enforcing the state rules.
//
// A disabled or destroyed version is FailedPrecondition, not NotFound: the
// version exists, and telling a caller it does not would send them looking
// for a creation bug instead of an enable call.
func (s *Store) AccessVersion(project, id, version string) (Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	number, err := s.resolveVersion(project, id, version)
	if err != nil {
		return Version{}, err
	}
	v, err := s.getVersion(project, id, number)
	if err != nil {
		return Version{}, err
	}
	if !v.Accessible() {
		return Version{}, apierror.FailedPrecondition(
			"secret version %s is %s and cannot be accessed", v.Name, v.State)
	}
	return v, nil
}

// ListVersions returns a secret's versions, newest first.
func (s *Store) ListVersions(project, id string) ([]Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.getSecret(project, id); err != nil {
		return nil, err
	}
	versions, err := s.listVersions(project, id)
	if err != nil {
		return nil, err
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Number > versions[j].Number })
	return versions, nil
}

func (s *Store) listVersions(project, id string) ([]Version, error) {
	keys, err := s.db.List(fmt.Sprintf("version/%s/%s/", project, id))
	if err != nil {
		return nil, apierror.Internal(err, "list versions")
	}
	out := make([]Version, 0, len(keys))
	for _, k := range keys {
		raw, err := s.db.Get(k)
		if err != nil {
			continue
		}
		var v Version
		if err := json.Unmarshal(raw, &v); err != nil {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// SetVersionState enables or disables a version.
func (s *Store) SetVersionState(project, id, version string, state VersionState) (Version, error) {
	if state != StateEnabled && state != StateDisabled {
		return Version{}, apierror.InvalidArgument("state must be ENABLED or DISABLED, got %s", state)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	number, err := s.resolveVersion(project, id, version)
	if err != nil {
		return Version{}, err
	}
	v, err := s.getVersion(project, id, number)
	if err != nil {
		return Version{}, err
	}
	// Destruction is terminal. Re-enabling would promise a payload that no
	// longer exists.
	if v.State == StateDestroyed {
		return Version{}, apierror.FailedPrecondition(
			"secret version %s is destroyed; its payload cannot be recovered", v.Name)
	}
	v.State = state
	v.Etag = s.etag()
	if err := s.put(versionKey(project, id, number), v); err != nil {
		return Version{}, err
	}
	return v, nil
}

// DestroyVersion discards a version's payload permanently.
func (s *Store) DestroyVersion(project, id, version string) (Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	number, err := s.resolveVersion(project, id, version)
	if err != nil {
		return Version{}, err
	}
	v, err := s.getVersion(project, id, number)
	if err != nil {
		return Version{}, err
	}
	if v.State == StateDestroyed {
		return Version{}, apierror.FailedPrecondition("secret version %s is already destroyed", v.Name)
	}

	v.State = StateDestroyed
	v.Destroyed = s.now().UTC()
	// The payload is cleared, not flagged. A destroyed version that still
	// held its bytes would leak them to anything reading storage directly.
	v.Payload = nil
	v.Etag = s.etag()
	if err := s.put(versionKey(project, id, number), v); err != nil {
		return Version{}, err
	}
	return v, nil
}

// Reset removes every secret in every project.
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, prefix := range []string{"secret/", "version/"} {
		keys, err := s.db.List(prefix)
		if err != nil {
			return apierror.Internal(err, "list %s", prefix)
		}
		for _, k := range keys {
			if err := s.db.Delete(k); err != nil {
				return apierror.Internal(err, "delete %s", k)
			}
		}
	}
	return nil
}

func (s *Store) put(key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return apierror.Internal(err, "encode %s", key)
	}
	if err := s.db.Put(key, raw); err != nil {
		return apierror.Internal(err, "store %s", key)
	}
	return nil
}
