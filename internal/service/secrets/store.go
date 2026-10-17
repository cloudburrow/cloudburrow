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
	"sync/atomic"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/store"
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
	// Replication records what the caller asked for: "automatic" or
	// "user-managed". Neither is enforced — there is one local store either
	// way — but both are kept and returned as set, so a caller reading the
	// secret back sees the policy it created (#857).
	Replication string `json:"replication"`
	// ReplicaLocations are a user-managed secret's replica locations, in the
	// order given. An automatic secret has none. Replication is immutable
	// after create, so nothing but CreateSecret writes this.
	ReplicaLocations []string `json:"replicaLocations,omitempty"`
	// KMSKeys are the customer-managed encryption keys (#1453): one entry
	// under automatic replication, one per replica under user-managed, ""
	// where a replica has none. nil when no replica has a key.
	KMSKeys []string `json:"kmsKeys,omitempty"`
	// NextVersion is the number the next AddSecretVersion will use. It never
	// decreases, so a destroyed version's number is never reused — reuse
	// would let a stale reference resolve to different bytes.
	NextVersion int    `json:"nextVersion"`
	Etag        string `json:"etag"`
	// IAMPolicy is stored, never enforced (ADR-0006, #365). Kept on the
	// secret so it is deleted, reset and snapshotted with it.
	IAMPolicy *iampolicy.Stored `json:"iamPolicy,omitempty"`
	// Expire is the secret's expire_time; zero means it never expires. A
	// ttl is stored as the expire_time it gives (#1225). At Expire the
	// secret and every version of it are deleted, on the store's clock.
	Expire time.Time `json:"expireTime,omitempty"`
	// Topics are the Pub/Sub topics, named in full, that the secret's
	// event notifications are published to (#1259).
	Topics []string `json:"topics,omitempty"`
	// NextRotation is rotation.next_rotation_time and RotationPeriod
	// rotation.rotation_period (#1278); zero is unset. At NextRotation a
	// SECRET_ROTATE notification goes to Topics, and NextRotation advances
	// by RotationPeriod, or is cleared when there is none.
	NextRotation   time.Time     `json:"nextRotationTime,omitempty"`
	RotationPeriod time.Duration `json:"rotationPeriod,omitempty"`
	// VersionDestroyTTL is version_destroy_ttl (#1278): with it, destroying
	// a version disables it and schedules its destruction this much later.
	VersionDestroyTTL time.Duration `json:"versionDestroyTtl,omitempty"`
	// ScheduledDestroys maps a version number to its scheduled_destroy_time,
	// so the sweep finds the versions due without reading every version.
	ScheduledDestroys map[string]time.Time `json:"scheduledDestroys,omitempty"`
}

// Expired reports whether the secret's expire_time has come at now.
func (s Secret) Expired(now time.Time) bool {
	return !s.Expire.IsZero() && !now.Before(s.Expire)
}

// Version is one version of a secret.
type Version struct {
	Name      string       `json:"name"`
	Number    int          `json:"number"`
	State     VersionState `json:"state"`
	Created   time.Time    `json:"created"`
	Destroyed time.Time    `json:"destroyed,omitempty"`
	// ScheduledDestroy is scheduled_destroy_time: a version destroyed on a
	// secret with a version_destroy_ttl is DISABLED until then (#1278).
	ScheduledDestroy time.Time `json:"scheduledDestroyTime,omitempty"`
	// Payload is the stored bytes. It is cleared on destroy rather than
	// marked, so a destroyed version cannot leak what it held.
	//
	// When a replica has a customer-managed key (#1453), it is the payload
	// sealed under a data key that Replicas hold encrypted, and PayloadSize
	// is the plaintext's length.
	Payload     []byte `json:"payload,omitempty"`
	PayloadSize int    `json:"payloadSize,omitempty"`
	Etag        string `json:"etag"`
	// Replicas are the version's replication_status: one entry under
	// automatic replication, one per replica under user-managed, with the
	// key version that encrypted each. Versions added before #1453 have none.
	Replicas []ReplicaStatus `json:"replicas,omitempty"`
	// ClientChecksum records that the client sent a CRC32C with the payload
	// and it matched; Secret Manager reports it as
	// client_specified_payload_checksum, and gcloud treats a version added
	// without it as possibly corrupted.
	ClientChecksum bool `json:"client_checksum,omitempty"`
}

// Accessible reports whether this version's payload can be read.
func (v Version) Accessible() bool { return v.State == StateEnabled }

// Store holds secrets and their versions.
type Store struct {
	mu    sync.Mutex
	db    store.Store
	now   func() time.Time
	clock sched.Clock
	// rearm wakes Run when an expire_time is set, so it waits for the new
	// earliest one.
	rearm chan struct{}
	// publish delivers event notifications (#1259); nil refuses topics.
	publish Publisher
	// offset is how far Advance has moved the store's clock past its base
	// clock, in nanoseconds (#1278).
	offset atomic.Int64
	// keys is Cloud KMS, for customer-managed encryption (#1453).
	keys func() KeyService
}

// NewStore returns a store backed by db.
func NewStore(db store.Store) *Store {
	return NewStoreOnClock(db, sched.RealClock{})
}

// NewStoreWithClock returns a store with an injected clock, so tests can
// assert on creation and destruction times without sleeping.
func NewStoreWithClock(db store.Store, now func() time.Time) *Store {
	return newStore(db, funcClock(now))
}

// NewStoreOnClock returns a store on clock, so a test can advance time past
// an expire_time and watch Run delete the secret.
func NewStoreOnClock(db store.Store, clock sched.Clock) *Store {
	return newStore(db, clock)
}

func newStore(db store.Store, clock sched.Clock) *Store {
	s := &Store{db: db, clock: clock, rearm: make(chan struct{}, 1)}
	s.now = func() time.Time { return clock.Now().Add(time.Duration(s.offset.Load())) }
	return s
}

// funcClock is a clock reading now, with real-time timers.
type funcClock func() time.Time

func (f funcClock) Now() time.Time                       { return f() }
func (funcClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

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
//
// A regional secret (#1519), projects/{project}/locations/{location}/secrets/{secret},
// parses too: its project is then the scope RegionalScope returns, which
// every Store method takes where it takes a project.
func ParseSecretName(name string) (project, secret string, err error) {
	parts := strings.Split(name, "/")
	if len(parts) == 6 && parts[0] == "projects" && parts[2] == "locations" && parts[4] == "secrets" {
		if !resource.ValidID(parts[3]) {
			return "", "", apierror.InvalidArgument("location %q is not a valid location ID", parts[3])
		}
		parts = []string{parts[0], RegionalScope(parts[1], parts[3]), parts[4], parts[5]}
	}
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "secrets" {
		return "", "", apierror.InvalidArgument(
			"%q is not a secret name; want projects/{project}/secrets/{secret} "+
				"or projects/{project}/locations/{location}/secrets/{secret}", name)
	}
	project, secret = parts[1], parts[3]
	if p, _ := SplitScope(project); !resource.ValidID(p) {
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

// SecretName renders a secret's resource name; a regional scope renders
// projects/{project}/locations/{location}/secrets/{secret}.
func SecretName(project, secret string) string {
	if p, l := SplitScope(project); l != "" {
		return fmt.Sprintf("projects/%s/locations/%s/%s%s", p, l, secretPrefix, secret)
	}
	return fmt.Sprintf("projects/%s/%s%s", project, secretPrefix, secret)
}

// regionalSep joins a project and a location into a regional secret's
// scope (#1519). No project ID or location contains it, and it is not a
// "/", so the storage keys keep their shape: a project's global listing
// (prefix secret/{project}/) never sees its regional secrets, and a
// location's listing (secret/{project}@{location}/) sees only its own.
const regionalSep = "@"

// RegionalScope is the scope of a project's regional secrets in location
// (secretmanager.{location}.rep.googleapis.com), which the Store takes
// wherever it takes a project.
func RegionalScope(project, location string) string { return project + regionalSep + location }

// SplitScope splits a scope into its project and, for a regional one, its
// location ("" for global).
func SplitScope(scope string) (project, location string) {
	project, location, _ = strings.Cut(scope, regionalSep)
	return project, location
}

// Regional reports whether s is a regional secret (#1519).
func (s Secret) Regional() bool {
	_, l := SplitScope(scopeOfName(s.Name))
	return l != ""
}

// scopeOfName is the scope of a secret's (or version's) name, "" if it is
// not one.
func scopeOfName(name string) string {
	p := strings.Split(name, "/")
	switch {
	case len(p) >= 6 && p[0] == "projects" && p[2] == "locations" && p[4] == "secrets":
		return RegionalScope(p[1], p[3])
	case len(p) >= 4 && p[0] == "projects" && p[2] == "secrets":
		return p[1]
	}
	return ""
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
//
// replication is "automatic" (the default when empty) or "user-managed";
// locations are a user-managed secret's replica locations, at least one, and
// an automatic secret takes none. A location given to an automatic secret is
// refused rather than dropped: the caller would read back a policy without
// the location it asked for.
func (s *Store) CreateSecret(project, id string, labels, annotations map[string]string, replication string, locations ...string) (Secret, error) {
	return s.CreateSecretExpiring(project, id, labels, annotations, time.Time{}, replication, locations...)
}

// CreateSecretExpiring is CreateSecret with an expire_time; zero is none.
// An expire_time that is not in the future is refused, as Google's
// expire_time is "the time at which the Secret is scheduled to expire"
// (documented, not measured against Google).
func (s *Store) CreateSecretExpiring(project, id string, labels, annotations map[string]string, expire time.Time, replication string, locations ...string) (Secret, error) {
	return s.CreateSecretWithTopics(project, id, labels, annotations, expire, nil, replication, locations...)
}

// CreateSecretWithTopics is CreateSecretExpiring with the Pub/Sub topics
// its event notifications go to (#1259). Each is sent TOPIC_CONFIGURED
// before the secret is stored, and a failed publish fails the create.
func (s *Store) CreateSecretWithTopics(project, id string, labels, annotations map[string]string, expire time.Time, topics []string, replication string, locations ...string) (Secret, error) {
	return s.CreateSecretScheduled(project, id, labels, annotations, expire, topics, Schedule{}, replication, locations...)
}

// CreateSecretScheduled is CreateSecretWithTopics with a rotation schedule
// and a version_destroy_ttl (#1278).
func (s *Store) CreateSecretScheduled(project, id string, labels, annotations map[string]string, expire time.Time, topics []string, sch Schedule, replication string, locations ...string) (Secret, error) {
	return s.CreateSecretEncrypted(project, id, labels, annotations, expire, topics, sch, nil, replication, locations...)
}

// CreateSecretEncrypted is CreateSecretScheduled with customer-managed
// encryption keys (#1453): keys[i] is replica i's, "" for none, and an
// automatic policy takes one. nil is none.
func (s *Store) CreateSecretEncrypted(project, id string, labels, annotations map[string]string, expire time.Time, topics []string, sch Schedule, keys []string, replication string, locations ...string) (Secret, error) {
	if err := sch.check(s.now(), true, true); err != nil {
		return Secret{}, err
	}
	if !sch.NextRotation.IsZero() && len(topics) == 0 {
		return Secret{}, errRotationTopics
	}
	if !expire.IsZero() && !expire.After(s.now()) {
		return Secret{}, apierror.InvalidArgument("expire_time %s is not in the future", expire.UTC().Format(time.RFC3339Nano))
	}
	if project == "" {
		return Secret{}, apierror.InvalidArgument("project must not be empty")
	}
	if err := ValidateSecretID(id); err != nil {
		return Secret{}, err
	}
	if replication == "" {
		replication = "automatic"
	}
	switch replication {
	case "automatic":
		if len(locations) > 0 {
			return Secret{}, apierror.InvalidArgument(
				"automatic replication takes no replica locations; use user-managed replication to name them")
		}
	case "user-managed":
		if len(locations) == 0 {
			return Secret{}, apierror.InvalidArgument("replication.user_managed.replicas must name at least one replica")
		}
		for _, l := range locations {
			if l == "" {
				return Secret{}, apierror.InvalidArgument("replication.user_managed.replicas[].location is required")
			}
		}
		locations = append([]string(nil), locations...)
	default:
		return Secret{}, apierror.InvalidArgument("replication %q must be automatic or user-managed", replication)
	}

	if keys != nil {
		want := 1
		if replication == "user-managed" {
			want = len(locations)
		}
		if len(keys) != want {
			return Secret{}, apierror.InvalidArgument("the replication policy has %d replicas; %d keys were given", want, len(keys))
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.checkKMSKeys(replication, locations, keys); err != nil {
		return Secret{}, err
	}
	if err := s.validateTopics(topics); err != nil {
		return Secret{}, err
	}
	key := secretKey(project, id)
	// An expired secret that the sweep has not reached yet is gone: the ID
	// is free again.
	if _, err := s.getSecret(project, id); err == nil {
		return Secret{}, apierror.AlreadyExists("secret %s already exists", SecretName(project, id))
	}
	if err := s.topicConfigured(topics); err != nil {
		return Secret{}, err
	}

	sec := Secret{
		Name:             SecretName(project, id),
		Created:          s.now().UTC(),
		Labels:           labels,
		Annotations:      annotations,
		Replication:      replication,
		ReplicaLocations: locations,
		KMSKeys:          normalKeys(keys),
		NextVersion:      1,
		Etag:             s.etag(),
	}
	if len(topics) > 0 {
		sec.Topics = append([]string(nil), topics...)
	}
	if !expire.IsZero() {
		sec.Expire = expire.UTC()
	}
	if !sch.NextRotation.IsZero() {
		sec.NextRotation = sch.NextRotation.UTC()
	}
	sec.RotationPeriod, sec.VersionDestroyTTL = sch.RotationPeriod, sch.VersionDestroyTTL
	if err := s.put(key, sec); err != nil {
		return Secret{}, err
	}
	s.notify(sec, EventSecretCreate, nil, "")
	if !sec.Expire.IsZero() || !sec.NextRotation.IsZero() {
		s.wake()
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
	// An expired secret is gone whether or not the sweep has deleted it
	// yet; it is deleted here so no read ever sees it.
	if sec.Expired(s.now()) {
		if err := s.deleteSecret(project, id); err != nil {
			return Secret{}, err
		}
		s.notify(sec, EventSecretDelete, nil, deleteExpiration)
		return Secret{}, apierror.NotFound("secret %s not found", SecretName(project, id))
	}
	return sec, nil
}

// UpdateSecret replaces the labels and annotations a caller may change.
//
// Only the mutable fields are touched: name, creation time and version
// numbering are not caller-settable, and accepting them silently would let a
// client believe it had renamed a secret.
func (s *Store) UpdateSecret(project, id string, labels, annotations map[string]string, updateLabels, updateAnnotations bool) (Secret, error) {
	return s.UpdateSecretFields(project, id, SecretUpdate{Labels: labels, Annotations: annotations,
		UpdateLabels: updateLabels, UpdateAnnotations: updateAnnotations})
}

// SecretUpdate is the mutable part of a secret and which of it to change.
type SecretUpdate struct {
	Labels, Annotations             map[string]string
	UpdateLabels, UpdateAnnotations bool
	// Expire is the new expire_time when UpdateExpire; zero clears it.
	Expire       time.Time
	UpdateExpire bool
	// Topics replace the secret's topics when UpdateTopics; empty clears
	// them (#1259).
	Topics       []string
	UpdateTopics bool
	// Schedule holds the new rotation and version_destroy_ttl; each Update
	// flag names the part of it that applies (#1278). A zero value clears.
	Schedule                                 Schedule
	UpdateNextRotation, UpdateRotationPeriod bool
	UpdateVersionDestroyTTL                  bool
}

// UpdateSecretFields applies u to a secret.
func (s *Store) UpdateSecretFields(project, id string, u SecretUpdate) (Secret, error) {
	if u.UpdateExpire && !u.Expire.IsZero() && !u.Expire.After(s.now()) {
		return Secret{}, apierror.InvalidArgument("expire_time %s is not in the future", u.Expire.UTC().Format(time.RFC3339Nano))
	}
	if err := u.Schedule.check(s.now(), u.UpdateNextRotation, u.UpdateVersionDestroyTTL); err != nil {
		return Secret{}, err
	}
	labels, annotations, updateLabels, updateAnnotations := u.Labels, u.Annotations, u.UpdateLabels, u.UpdateAnnotations
	s.mu.Lock()
	defer s.mu.Unlock()

	sec, err := s.getSecret(project, id)
	if err != nil {
		return Secret{}, err
	}
	next, period := sec.NextRotation, sec.RotationPeriod
	if u.UpdateNextRotation {
		next = u.Schedule.NextRotation.UTC()
	}
	if u.UpdateRotationPeriod {
		period = u.Schedule.RotationPeriod
	}
	if period > 0 && next.IsZero() {
		return Secret{}, errPeriodNeedsNext
	}
	topics := sec.Topics
	if u.UpdateTopics {
		topics = u.Topics
	}
	if !next.IsZero() && len(topics) == 0 {
		return Secret{}, errRotationTopics
	}
	if u.UpdateTopics {
		if err := s.validateTopics(u.Topics); err != nil {
			return Secret{}, err
		}
		// TOPIC_CONFIGURED goes to every topic on the secret, including
		// ones that were already there, before the update is stored.
		if err := s.topicConfigured(u.Topics); err != nil {
			return Secret{}, err
		}
		sec.Topics = nil
		if len(u.Topics) > 0 {
			sec.Topics = append([]string(nil), u.Topics...)
		}
	}
	if u.UpdateExpire {
		sec.Expire = u.Expire.UTC()
		if u.Expire.IsZero() {
			sec.Expire = time.Time{}
		}
		defer s.wake()
	}
	if u.UpdateNextRotation || u.UpdateRotationPeriod {
		if next.IsZero() {
			next = time.Time{}
		}
		sec.NextRotation, sec.RotationPeriod = next, period
		defer s.wake()
	}
	if u.UpdateVersionDestroyTTL {
		// Versions already scheduled keep their schedule, as Google
		// documents; the new ttl applies to later destroys.
		sec.VersionDestroyTTL = u.Schedule.VersionDestroyTTL
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
	s.notify(sec, EventSecretUpdate, nil, "")
	return sec, nil
}

// GetIamPolicy returns a secret's stored policy, nil when none was set.
func (s *Store) GetIamPolicy(project, id string) (*iampolicy.Stored, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec, err := s.getSecret(project, id)
	if err != nil {
		return nil, err
	}
	return sec.IAMPolicy, nil
}

// SetIamPolicy applies a SetIamPolicy request under the lock, so two
// read-modify-write loops cannot both succeed against the same etag.
func (s *Store) SetIamPolicy(project, id string, req *iampb.SetIamPolicyRequest) (iampolicy.Stored, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec, err := s.getSecret(project, id)
	if err != nil {
		return iampolicy.Stored{}, err
	}
	next, err := iampolicy.Set(sec.IAMPolicy, req)
	if err != nil {
		return iampolicy.Stored{}, err
	}
	sec.IAMPolicy = &next
	if err := s.put(secretKey(project, id), sec); err != nil {
		return iampolicy.Stored{}, err
	}
	return next, nil
}

// ListSecrets returns a project's secrets, ordered by name.
func (s *Store) ListSecrets(project string) ([]Secret, error) {
	if project == "" {
		return nil, apierror.InvalidArgument("project must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	values, err := s.scan("secret/" + project + "/")
	if err != nil {
		return nil, apierror.Internal(err, "list secrets")
	}
	out := make([]Secret, 0, len(values))
	for _, raw := range values {
		var sec Secret
		if err := json.Unmarshal(raw, &sec); err != nil {
			continue
		}
		if sec.Expired(s.now()) {
			project, id := secretParts(sec.Name)
			if err := s.deleteSecret(project, id); err != nil {
				return nil, err
			}
			continue
		}
		out = append(out, sec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListRegionalSecrets returns a project's regional secrets in every
// location (#1519), ordered by name. An expired one is left out; the sweep
// deletes it.
func (s *Store) ListRegionalSecrets(project string) ([]Secret, error) {
	if project == "" {
		return nil, apierror.InvalidArgument("project must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	values, err := s.scan("secret/" + project + regionalSep)
	if err != nil {
		return nil, apierror.Internal(err, "list regional secrets")
	}
	out := make([]Secret, 0, len(values))
	for _, raw := range values {
		var sec Secret
		if json.Unmarshal(raw, &sec) != nil || sec.Expired(s.now()) {
			continue
		}
		if p, l := SplitScope(scopeOfName(sec.Name)); p != project || l == "" {
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

	sec, err := s.getSecret(project, id)
	if err != nil {
		return err
	}
	if err := s.deleteSecret(project, id); err != nil {
		return err
	}
	s.notify(sec, EventSecretDelete, nil, deleteRequested)
	return nil
}

// deleteSecret removes a secret and its versions. The caller holds s.mu.
func (s *Store) deleteSecret(project, id string) error {
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
	if err := s.deleteMounts(project, id); err != nil {
		return err
	}
	return nil
}

// MaxPayloadBytes is the documented Secret Manager payload limit.
const MaxPayloadBytes = 64 * 1024

// AddVersion stores a new payload and returns the version created.
func (s *Store) AddVersion(project, id string, payload []byte) (Version, error) {
	return s.addVersion(project, id, payload, false)
}

// AddVersionWithChecksum is AddVersion for a payload whose client-supplied
// CRC32C has already been verified, which the version then reports.
func (s *Store) AddVersionWithChecksum(project, id string, payload []byte) (Version, error) {
	return s.addVersion(project, id, payload, true)
}

func (s *Store) addVersion(project, id string, payload []byte, clientChecksum bool) (Version, error) {
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
	v := Version{
		Name:    VersionName(project, id, number),
		Number:  number,
		State:   StateEnabled,
		Created: s.now().UTC(),
		Etag:    s.etag(),

		ClientChecksum: clientChecksum,
	}
	// Encrypted before anything is stored: a key that cannot encrypt
	// fails the add and uses no version number.
	if err := s.sealVersion(sec, &v, payload); err != nil {
		return Version{}, err
	}
	sec.NextVersion++
	sec.Etag = s.etag()
	if err := s.put(secretKey(project, id), sec); err != nil {
		return Version{}, err
	}
	if err := s.put(versionKey(project, id, number), v); err != nil {
		return Version{}, err
	}
	s.notify(sec, EventSecretVersionAdd, &v, "")
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
	return s.openVersion(v)
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
	values, err := s.scan(fmt.Sprintf("version/%s/%s/", project, id))
	if err != nil {
		return nil, apierror.Internal(err, "list versions")
	}
	out := make([]Version, 0, len(values))
	for _, raw := range values {
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
	// Enabling or disabling a version scheduled for destruction cancels
	// the destruction, as Google documents (#1278).
	cancelled := !v.ScheduledDestroy.IsZero()
	v.ScheduledDestroy = time.Time{}
	if err := s.put(versionKey(project, id, number), v); err != nil {
		return Version{}, err
	}
	if cancelled {
		if err := s.unschedule(project, id, number); err != nil {
			return Version{}, err
		}
	}
	if state == StateDisabled {
		// A disabled version cannot be read by an instance that starts.
		if err := s.dropMount(project, id, v); err != nil {
			return Version{}, err
		}
	}
	event := EventSecretVersionEnable
	if state == StateDisabled {
		event = EventSecretVersionDisable
	}
	s.notifyVersion(project, id, event, v)
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
	if !v.ScheduledDestroy.IsZero() {
		return Version{}, apierror.FailedPrecondition("secret version %s is already scheduled for destruction at %s; "+
			"enable or disable it to cancel that", v.Name, v.ScheduledDestroy.UTC().Format(time.RFC3339Nano))
	}
	sec, err := s.getSecret(project, id)
	if err != nil {
		return Version{}, err
	}
	if sec.VersionDestroyTTL > 0 {
		return s.scheduleDestroy(sec, v)
	}
	return s.destroyNow(project, id, v)
}

// destroyNow destroys v at once. The caller holds s.mu.
func (s *Store) destroyNow(project, id string, v Version) (Version, error) {
	// The payload is cleared, not flagged. A destroyed version that still
	// held its bytes would leak them to anything reading storage directly.
	v, err := s.destroyNowQuiet(project, id, v)
	if err != nil {
		return Version{}, err
	}
	s.notifyVersion(project, id, EventSecretVersionDestroy, v)
	return v, nil
}

// scanner is a store that reads every value under a prefix with one
// request; KubeStore does, with one kubectl list.
type scanner interface {
	Scan(prefix string) (map[string][]byte, error)
}

// scan returns the values under prefix: with one request when the store is
// a scanner, and otherwise with List and a Get per key (the file and memory
// stores, where a Get costs nothing), leaving out a key a Get fails on. The
// caller orders what it decodes.
func (s *Store) scan(prefix string) (map[string][]byte, error) {
	if sc, ok := s.db.(scanner); ok {
		return sc.Scan(prefix)
	}
	keys, err := s.db.List(prefix)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(keys))
	for _, k := range keys {
		if raw, err := s.db.Get(k); err == nil {
			out[k] = raw
		}
	}
	return out, nil
}

// bulkDeleter is a store that removes every record with one request;
// KubeStore does, with one DeleteCollection.
type bulkDeleter interface{ DeleteAll() error }

// Reset removes every secret in every project.
//
// Over Kubernetes it is one DeleteCollection. Listing every key and deleting
// each, a version's delete a read and a rewrite of its Secret, held s.mu
// across requests whose number grew with what earlier runs left (#1232).
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if bd, ok := s.db.(bulkDeleter); ok {
		if err := bd.DeleteAll(); err != nil {
			return apierror.Internal(err, "delete every secret")
		}
		return nil
	}
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

// ResolveSecretRef resolves a Cloud Run secret reference to the Kubernetes
// Secret and data key that hold its payload.
//
// It implements the resolver the Cloud Run adapter needs for `secretKeyRef`
// environment variables, and it resolves rather than forwards: Kubernetes has
// no "latest" key, so the alias must become a concrete version number at
// deployment time.
//
// The consequence is stated rather than hidden: a revision pins the version
// that was latest when it was created, and adding a version later does not
// change a running revision. Cloud Run behaves the same way for environment
// variables, so matching it is also the more faithful choice.
func (s *Store) ResolveSecretRef(project, secret, version string) (secretName, dataKey string, err error) {
	// A reference may be a bare ID or a full resource name. The full form
	// wins, because it says which project it means.
	id := secret
	if strings.Contains(secret, "/") {
		refProject, refID, parseErr := ParseSecretName(secret)
		if parseErr != nil {
			return "", "", parseErr
		}
		project, id = refProject, refID
	}
	if project == "" {
		return "", "", apierror.InvalidArgument(
			"secret reference %q has no project and none could be inferred", secret)
	}
	if version == "" {
		version = LatestAlias
	}

	number, err := s.ResolveVersion(project, id, version)
	if err != nil {
		return "", "", err
	}
	v, err := s.GetVersion(project, id, strconv.Itoa(number))
	if err != nil {
		return "", "", err
	}
	// A disabled or destroyed version has no data key, so the pod would fail
	// to start with Kubernetes complaining about a missing key — a message
	// that points nowhere near the cause.
	if !v.Accessible() {
		return "", "", apierror.FailedPrecondition(
			"secret version %s is %s and cannot be mounted", v.Name, v.State)
	}
	// The Kubernetes Secret holds what is stored, which for a version
	// under a customer-managed key (#1453) is ciphertext: the instance
	// decrypts it into the mount Secret instead (#1489).
	if v.encrypted() {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.resolveEncrypted(project, id, v)
	}
	return KubernetesSecretName(project, id), VersionKey(number), nil
}
