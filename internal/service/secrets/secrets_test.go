package secrets

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/identity-wael/cloudburrow/internal/apierror"
	"github.com/identity-wael/cloudburrow/internal/store"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db := store.NewMemory()
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	return NewStoreWithClock(db, func() time.Time {
		now = now.Add(time.Second)
		return now
	})
}

// seed creates a secret with the given payloads, returning the versions.
func seed(t *testing.T, s *Store, project, id string, payloads ...string) []Version {
	t.Helper()
	if _, err := s.CreateSecret(project, id, nil, nil, ""); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	var out []Version
	for _, p := range payloads {
		v, err := s.AddVersion(project, id, []byte(p))
		if err != nil {
			t.Fatalf("AddVersion(%q): %v", p, err)
		}
		out = append(out, v)
	}
	return out
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got no error", want)
	}
	if got := apierror.From(err).Code; got != want {
		t.Errorf("code = %s, want %s (%v)", got, want, err)
	}
}

// --- names -------------------------------------------------------------

func TestParseSecretName(t *testing.T) {
	t.Parallel()
	project, secret, err := ParseSecretName("projects/demo/secrets/api-key")
	if err != nil {
		t.Fatalf("ParseSecretName: %v", err)
	}
	if project != "demo" || secret != "api-key" {
		t.Errorf("parsed %q/%q", project, secret)
	}

	for _, bad := range []string{
		"", "secrets/api-key", "projects/demo", "projects/demo/topics/x",
		"projects/demo/secrets/", "projects/demo/secrets/../etc",
	} {
		if _, _, err := ParseSecretName(bad); err == nil {
			t.Errorf("ParseSecretName(%q) accepted an invalid name", bad)
		}
	}
}

func TestParseVersionNameAcceptsNumbersAndLatest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, want string }{
		{"projects/demo/secrets/k/versions/1", "1"},
		{"projects/demo/secrets/k/versions/42", "42"},
		{"projects/demo/secrets/k/versions/latest", "latest"},
	} {
		_, _, version, err := ParseVersionName(tc.name)
		if err != nil {
			t.Errorf("ParseVersionName(%q): %v", tc.name, err)
			continue
		}
		if version != tc.want {
			t.Errorf("version = %q, want %q", version, tc.want)
		}
	}

	for _, bad := range []string{
		"projects/demo/secrets/k",
		"projects/demo/secrets/k/versions/",
		"projects/demo/secrets/k/versions/0",
		"projects/demo/secrets/k/versions/-1",
		"projects/demo/secrets/k/versions/newest",
		"projects/demo/secrets/k/versions/1/extra",
	} {
		if _, _, _, err := ParseVersionName(bad); err == nil {
			t.Errorf("ParseVersionName(%q) accepted an invalid name", bad)
		}
	}
}

func TestValidateSecretIDFollowsTheDocumentedRules(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"a", "API_KEY", "api-key-1", strings.Repeat("a", 255)} {
		if err := ValidateSecretID(ok); err != nil {
			t.Errorf("ValidateSecretID(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "has space", "has.dot", "has/slash", strings.Repeat("a", 256)} {
		err := ValidateSecretID(bad)
		if err == nil {
			t.Errorf("ValidateSecretID(%q) accepted it", bad)
			continue
		}
		wantCode(t, err, codes.InvalidArgument)
	}
}

// Secret Manager allows characters Kubernetes does not, so the mapping must
// be both valid and collision-free.
func TestKubernetesSecretNameIsValidAndDistinct(t *testing.T) {
	t.Parallel()
	a := KubernetesSecretName("demo", "My_Secret")
	b := KubernetesSecretName("demo", "my-secret")
	if a == b {
		t.Errorf("two different secrets map to the same Kubernetes name: %q", a)
	}

	// The same ID in two projects must not share an object either.
	if KubernetesSecretName("one", "k") == KubernetesSecretName("two", "k") {
		t.Error("the same secret ID in two projects maps to one Kubernetes name")
	}

	for _, name := range []string{a, b, KubernetesSecretName("demo", "___"), KubernetesSecretName("p", strings.Repeat("A", 255))} {
		if len(name) > 253 {
			t.Errorf("name is %d characters, over the Kubernetes limit: %q", len(name), name)
		}
		for _, r := range name {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			default:
				t.Errorf("name %q contains %q, which Kubernetes rejects", name, r)
			}
		}
		if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
			t.Errorf("name %q starts or ends with a hyphen", name)
		}
	}

	// The mapping must be stable: a name that changed between runs would
	// orphan the object holding the payload.
	if KubernetesSecretName("demo", "My_Secret") != a {
		t.Error("KubernetesSecretName is not deterministic")
	}
}

// --- secrets -----------------------------------------------------------

func TestCreateGetAndListSecrets(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	sec, err := s.CreateSecret("demo", "api-key", map[string]string{"env": "dev"}, nil, "")
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if sec.Name != "projects/demo/secrets/api-key" {
		t.Errorf("name = %q", sec.Name)
	}
	if sec.Replication != "automatic" {
		t.Errorf("replication = %q, want the default automatic", sec.Replication)
	}
	if sec.Etag == "" {
		t.Error("a created secret has no etag")
	}

	got, err := s.GetSecret("demo", "api-key")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if got.Labels["env"] != "dev" {
		t.Errorf("labels = %v", got.Labels)
	}

	// Another project's identically named secret must not appear.
	if _, err := s.CreateSecret("other", "api-key", nil, nil, ""); err != nil {
		t.Fatalf("CreateSecret in another project: %v", err)
	}
	list, err := s.ListSecrets("demo")
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListSecrets returned %d secrets, want 1", len(list))
	}
}

func TestDuplicateSecretIsAlreadyExists(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seed(t, s, "demo", "k")
	_, err := s.CreateSecret("demo", "k", nil, nil, "")
	wantCode(t, err, codes.AlreadyExists)
}

func TestMissingSecretIsNotFound(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, err := s.GetSecret("demo", "nope")
	wantCode(t, err, codes.NotFound)

	wantCode(t, s.DeleteSecret("demo", "nope"), codes.NotFound)

	_, err = s.AddVersion("demo", "nope", []byte("x"))
	wantCode(t, err, codes.NotFound)
}

// A user-managed request is recorded rather than silently rewritten, so a
// caller reading it back sees what they set.
func TestUserManagedReplicationIsRecorded(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	sec, err := s.CreateSecret("demo", "k", nil, nil, "user-managed")
	if err != nil {
		t.Fatal(err)
	}
	if sec.Replication != "user-managed" {
		t.Errorf("replication = %q", sec.Replication)
	}
}

func TestUpdateSecretTouchesOnlyWhatTheMaskNames(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, err := s.CreateSecret("demo", "k",
		map[string]string{"a": "1"}, map[string]string{"note": "keep"}, ""); err != nil {
		t.Fatal(err)
	}

	updated, err := s.UpdateSecret("demo", "k", map[string]string{"a": "2"}, nil, true, false)
	if err != nil {
		t.Fatalf("UpdateSecret: %v", err)
	}
	if updated.Labels["a"] != "2" {
		t.Errorf("labels = %v", updated.Labels)
	}
	if updated.Annotations["note"] != "keep" {
		t.Errorf("annotations were cleared although the mask did not name them: %v", updated.Annotations)
	}
	if updated.Name != "projects/demo/secrets/k" {
		t.Errorf("name changed: %q", updated.Name)
	}
}

// An orphaned version whose secret is gone could be listed by a later secret
// with the same ID, handing a caller another secret's bytes.
func TestDeleteSecretRemovesItsVersions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seed(t, s, "demo", "k", "one", "two")

	if err := s.DeleteSecret("demo", "k"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if _, err := s.CreateSecret("demo", "k", nil, nil, ""); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	versions, err := s.ListVersions("demo", "k")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 0 {
		t.Errorf("a recreated secret inherited %d versions", len(versions))
	}
}

// --- versions ----------------------------------------------------------

func TestVersionNumbersIncrementAndNeverRepeat(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	versions := seed(t, s, "demo", "k", "one", "two", "three")

	for i, v := range versions {
		if v.Number != i+1 {
			t.Errorf("version %d has number %d", i, v.Number)
		}
		if v.State != StateEnabled {
			t.Errorf("a new version is %s, want ENABLED", v.State)
		}
	}

	// Destroying the latest must not free its number for reuse: a stale
	// reference would then resolve to different bytes.
	if _, err := s.DestroyVersion("demo", "k", "3"); err != nil {
		t.Fatal(err)
	}
	next, err := s.AddVersion("demo", "k", []byte("four"))
	if err != nil {
		t.Fatal(err)
	}
	if next.Number != 4 {
		t.Errorf("next version = %d, want 4; a destroyed number was reused", next.Number)
	}
}

func TestAccessReturnsThePayload(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seed(t, s, "demo", "k", "first", "second")

	v, err := s.AccessVersion("demo", "k", "1")
	if err != nil {
		t.Fatalf("AccessVersion: %v", err)
	}
	if string(v.Payload) != "first" {
		t.Errorf("payload = %q", v.Payload)
	}
}

// The contract defines latest as the most recently *created* version.
// Skipping a disabled latest would hand a caller older bytes than they asked
// for without saying so.
func TestLatestIsTheMostRecentlyCreatedNotTheMostRecentlyEnabled(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seed(t, s, "demo", "k", "old", "new")

	v, err := s.AccessVersion("demo", "k", LatestAlias)
	if err != nil {
		t.Fatalf("AccessVersion(latest): %v", err)
	}
	if string(v.Payload) != "new" {
		t.Errorf("latest returned %q", v.Payload)
	}

	if _, err := s.SetVersionState("demo", "k", "2", StateDisabled); err != nil {
		t.Fatal(err)
	}
	_, err = s.AccessVersion("demo", "k", LatestAlias)
	wantCode(t, err, codes.FailedPrecondition)
	if err != nil && strings.Contains(err.Error(), "old") {
		t.Error("a disabled latest silently fell back to an older version")
	}
}

// The version exists; saying it does not would send a caller looking for a
// creation bug instead of an enable call.
func TestDisabledVersionIsFailedPreconditionNotNotFound(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seed(t, s, "demo", "k", "secret")

	if _, err := s.SetVersionState("demo", "k", "1", StateDisabled); err != nil {
		t.Fatal(err)
	}
	_, err := s.AccessVersion("demo", "k", "1")
	wantCode(t, err, codes.FailedPrecondition)

	// Metadata is still readable, as it is on Google.
	v, err := s.GetVersion("demo", "k", "1")
	if err != nil {
		t.Fatalf("GetVersion on a disabled version: %v", err)
	}
	if v.State != StateDisabled {
		t.Errorf("state = %s", v.State)
	}

	// Re-enabling restores access.
	if _, err := s.SetVersionState("demo", "k", "1", StateEnabled); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AccessVersion("demo", "k", "1"); err != nil {
		t.Errorf("a re-enabled version is still inaccessible: %v", err)
	}
}

// Destruction is terminal, and the payload must actually be gone rather than
// flagged — a stored payload would leak to anything reading storage directly.
func TestDestroyIsTerminalAndClearsThePayload(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seed(t, s, "demo", "k", "sensitive")

	v, err := s.DestroyVersion("demo", "k", "1")
	if err != nil {
		t.Fatalf("DestroyVersion: %v", err)
	}
	if v.State != StateDestroyed {
		t.Errorf("state = %s", v.State)
	}
	if v.Destroyed.IsZero() {
		t.Error("no destroy time was recorded")
	}
	if len(v.Payload) != 0 {
		t.Errorf("the payload survived destruction: %q", v.Payload)
	}

	stored, err := s.GetVersion("demo", "k", "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Payload) != 0 {
		t.Errorf("the stored payload survived destruction: %q", stored.Payload)
	}

	_, err = s.AccessVersion("demo", "k", "1")
	wantCode(t, err, codes.FailedPrecondition)

	// Re-enabling would promise a payload that no longer exists.
	_, err = s.SetVersionState("demo", "k", "1", StateEnabled)
	wantCode(t, err, codes.FailedPrecondition)

	_, err = s.DestroyVersion("demo", "k", "1")
	wantCode(t, err, codes.FailedPrecondition)
}

func TestListVersionsIsNewestFirst(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seed(t, s, "demo", "k", "a", "b", "c")

	versions, err := s.ListVersions("demo", "k")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("got %d versions", len(versions))
	}
	for i := 1; i < len(versions); i++ {
		if versions[i-1].Number <= versions[i].Number {
			t.Fatalf("versions are not newest first: %v", versions)
		}
	}
}

func TestPayloadLimitsAreEnforced(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seed(t, s, "demo", "k")

	_, err := s.AddVersion("demo", "k", nil)
	wantCode(t, err, codes.InvalidArgument)

	_, err = s.AddVersion("demo", "k", make([]byte, MaxPayloadBytes+1))
	wantCode(t, err, codes.InvalidArgument)

	if _, err := s.AddVersion("demo", "k", make([]byte, MaxPayloadBytes)); err != nil {
		t.Errorf("a payload at exactly the limit was refused: %v", err)
	}
}

func TestLatestOnASecretWithNoVersionsIsNotFound(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seed(t, s, "demo", "k")

	_, err := s.AccessVersion("demo", "k", LatestAlias)
	wantCode(t, err, codes.NotFound)
}

func TestMissingVersionIsNotFound(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seed(t, s, "demo", "k", "one")

	_, err := s.AccessVersion("demo", "k", "99")
	wantCode(t, err, codes.NotFound)
}

func TestSetVersionStateRejectsDestroyed(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seed(t, s, "demo", "k", "x")

	_, err := s.SetVersionState("demo", "k", "1", StateDestroyed)
	wantCode(t, err, codes.InvalidArgument)
}

// A stored payload is a copy: a caller mutating the slice it passed in must
// not change what was stored.
func TestStoredPayloadIsACopy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seed(t, s, "demo", "k")

	payload := []byte("original")
	if _, err := s.AddVersion("demo", "k", payload); err != nil {
		t.Fatal(err)
	}
	copy(payload, "mutated!")

	v, err := s.AccessVersion("demo", "k", "1")
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Payload) != "original" {
		t.Errorf("stored payload = %q; the caller's mutation reached the store", v.Payload)
	}
}

func TestResetRemovesEverything(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seed(t, s, "demo", "k", "a")
	seed(t, s, "other", "j", "b")

	if err := s.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for _, project := range []string{"demo", "other"} {
		list, err := s.ListSecrets(project)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 0 {
			t.Errorf("%s still has %d secrets after reset", project, len(list))
		}
	}
}
