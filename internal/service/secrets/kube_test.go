package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
)

// fakeCluster is an in-memory stand-in for the Kubernetes API, so every
// KubeStore path is exercised without a cluster.
type fakeCluster struct {
	objects map[string]map[string]any
	calls   []string
	failGet bool
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{objects: map[string]map[string]any{}}
}

func (f *fakeCluster) Run(_ context.Context, stdin string, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))

	switch {
	case contains(args, "apply"):
		var obj map[string]any
		if err := json.Unmarshal([]byte(stdin), &obj); err != nil {
			return "", fmt.Errorf("apply received invalid JSON: %w", err)
		}
		meta, _ := obj["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if name == "" {
			return "", errors.New("apply received an object with no name")
		}
		f.objects[name] = obj
		return "secret/" + name + " configured\n", nil

	case contains(args, "delete"):
		name := args[len(args)-2]
		delete(f.objects, name)
		return "", nil

	case contains(args, "get") && contains(args, "-l"):
		items := make([]any, 0, len(f.objects))
		for _, o := range f.objects {
			items = append(items, o)
		}
		out, _ := json.Marshal(map[string]any{"items": items})
		return string(out), nil

	case contains(args, "get"):
		if f.failGet {
			return "", errors.New("the server could not find the requested resource")
		}
		// `kubectl get secret <name> -o json`
		var name string
		for i, a := range args {
			if a == "secret" && i+1 < len(args) {
				name = args[i+1]
			}
		}
		obj, ok := f.objects[name]
		if !ok {
			return "", fmt.Errorf(`secrets "%s" not found (NotFound)`, name)
		}
		out, _ := json.Marshal(obj)
		return string(out), nil
	}
	return "", fmt.Errorf("unexpected kubectl invocation: %v", args)
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func (f *fakeCluster) object(t *testing.T, project, id string) map[string]any {
	t.Helper()
	obj, ok := f.objects[KubernetesSecretName(project, id)]
	if !ok {
		t.Fatalf("no Kubernetes Secret for %s/%s; have %v", project, id, f.names())
	}
	return obj
}

func (f *fakeCluster) names() []string {
	out := make([]string, 0, len(f.objects))
	for n := range f.objects {
		out = append(out, n)
	}
	return out
}

func newKubeBackedStore(t *testing.T) (*Store, *fakeCluster) {
	t.Helper()
	fake := newFakeCluster()
	return NewStore(NewKubeStore(fake, "cloudburrow", "test")), fake
}

// The payload must land in `data`, because that is the only place a pod can
// reference through secretKeyRef.
func TestPayloadLandsInKubernetesData(t *testing.T) {
	t.Parallel()
	s, fake := newKubeBackedStore(t)

	if _, err := s.CreateSecret("demo", "api-key", nil, nil, ""); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if _, err := s.AddVersion("demo", "api-key", []byte("s3cret")); err != nil {
		t.Fatalf("AddVersion: %v", err)
	}

	obj := fake.object(t, "demo", "api-key")
	data, _ := obj["data"].(map[string]any)
	encoded, ok := data[VersionKey(1)].(string)
	if !ok {
		t.Fatalf("no data key %q; data = %v", VersionKey(1), data)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("data value is not base64: %v", err)
	}
	if string(raw) != "s3cret" {
		t.Errorf("data holds %q", raw)
	}
}

// Leaving a copy in the annotation would put the bytes somewhere `kubectl
// describe` prints them.
func TestPayloadIsNotDuplicatedIntoAnnotations(t *testing.T) {
	t.Parallel()
	s, fake := newKubeBackedStore(t)
	seedKube(t, s, "demo", "k", "s3cret")

	obj := fake.object(t, "demo", "k")
	meta, _ := obj["metadata"].(map[string]any)
	annotations, _ := meta["annotations"].(map[string]any)
	for name, value := range annotations {
		str, _ := value.(string)
		if strings.Contains(str, "s3cret") || strings.Contains(str, base64.StdEncoding.EncodeToString([]byte("s3cret"))) {
			t.Errorf("annotation %q carries the payload: %s", name, str)
		}
	}
}

func TestOwnershipLabelsAreApplied(t *testing.T) {
	t.Parallel()
	s, fake := newKubeBackedStore(t)
	seedKube(t, s, "demo", "k", "x")

	obj := fake.object(t, "demo", "k")
	meta, _ := obj["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)

	if labels[OwnerLabel] != "true" {
		t.Errorf("%s = %v; without it cleanup cannot tell this object is ours",
			OwnerLabel, labels[OwnerLabel])
	}
	if labels[ServiceLabel] != ServiceLabelValue {
		t.Errorf("%s = %v", ServiceLabel, labels[ServiceLabel])
	}
	if labels[ProjectLabel] != "demo" {
		t.Errorf("%s = %v", ProjectLabel, labels[ProjectLabel])
	}
	if meta["namespace"] != "cloudburrow" {
		t.Errorf("namespace = %v, want the managed namespace", meta["namespace"])
	}
}

// The Kubernetes object name is lossy, so the original ID must survive
// somewhere or a listing could not reconstruct the resource name.
func TestOriginalSecretIDSurvivesTheLossyName(t *testing.T) {
	t.Parallel()
	s, fake := newKubeBackedStore(t)
	seedKube(t, s, "demo", "My_Secret", "x")

	obj := fake.object(t, "demo", "My_Secret")
	meta, _ := obj["metadata"].(map[string]any)
	annotations, _ := meta["annotations"].(map[string]any)
	if annotations[SecretIDAnnotation] != "My_Secret" {
		t.Errorf("%s = %v, want the original ID", SecretIDAnnotation, annotations[SecretIDAnnotation])
	}

	// And a round trip through the store must return the original name.
	list, err := s.ListSecrets("demo")
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(list) != 1 || list[0].Name != "projects/demo/secrets/My_Secret" {
		t.Errorf("listing = %+v", list)
	}
}

// A label value cannot hold every project ID, so an unsafe one is sanitised
// rather than rejected — and the authoritative value stays in an annotation.
func TestUnsafeProjectIsSanitisedInTheLabelAndKeptInAnAnnotation(t *testing.T) {
	t.Parallel()
	s, fake := newKubeBackedStore(t)
	seedKube(t, s, "My.Project_99", "k", "x")

	obj := fake.object(t, "My.Project_99", "k")
	meta, _ := obj["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)
	annotations, _ := meta["annotations"].(map[string]any)

	label, _ := labels[ProjectLabel].(string)
	if strings.ContainsAny(label, "/ ") || label != strings.ToLower(label) {
		t.Errorf("project label %q is not a valid label value", label)
	}
	if annotations["cloudburrow.dev/secret-project"] != "My.Project_99" {
		t.Errorf("the authoritative project was not preserved: %v",
			annotations["cloudburrow.dev/secret-project"])
	}
}

// A destroyed version's data key must actually go away, not hold an empty
// string a caller could read back.
func TestDestroyRemovesTheDataKey(t *testing.T) {
	t.Parallel()
	s, fake := newKubeBackedStore(t)
	seedKube(t, s, "demo", "k", "sensitive")

	if _, err := s.DestroyVersion("demo", "k", "1"); err != nil {
		t.Fatalf("DestroyVersion: %v", err)
	}
	obj := fake.object(t, "demo", "k")
	data, _ := obj["data"].(map[string]any)
	if _, present := data[VersionKey(1)]; present {
		t.Errorf("the data key survived destruction: %v", data)
	}

	v, err := s.GetVersion("demo", "k", "1")
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	if v.State != StateDestroyed {
		t.Errorf("state = %s", v.State)
	}
	if len(v.Payload) != 0 {
		t.Errorf("payload = %q", v.Payload)
	}
}

func TestFullLifecycleThroughKubernetes(t *testing.T) {
	t.Parallel()
	s, fake := newKubeBackedStore(t)

	seedKube(t, s, "demo", "k", "one")
	if _, err := s.AddVersion("demo", "k", []byte("two")); err != nil {
		t.Fatal(err)
	}

	v, err := s.AccessVersion("demo", "k", LatestAlias)
	if err != nil {
		t.Fatalf("AccessVersion: %v", err)
	}
	if string(v.Payload) != "two" {
		t.Errorf("latest = %q", v.Payload)
	}

	versions, err := s.ListVersions("demo", "k")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("got %d versions", len(versions))
	}

	// Deleting the secret must remove the whole object, not leave an empty
	// shell that a later listing would pick up.
	if err := s.DeleteSecret("demo", "k"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if len(fake.objects) != 0 {
		t.Errorf("deletion left %v behind", fake.names())
	}
}

// Two secrets must not share one object, and a listing must not mix them.
func TestSecretsAreIsolatedPerProject(t *testing.T) {
	t.Parallel()
	s, _ := newKubeBackedStore(t)
	seedKube(t, s, "one", "k", "first")
	seedKube(t, s, "two", "k", "second")

	a, err := s.AccessVersion("one", "k", "1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.AccessVersion("two", "k", "1")
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Payload) == string(b.Payload) {
		t.Error("two projects' secrets share one payload")
	}

	list, err := s.ListSecrets("one")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("listing one project returned %d secrets", len(list))
	}
}

// An object carrying our labels but not our annotations is not ours to
// interpret, and guessing at it could surface someone else's data as a secret.
func TestListIgnoresObjectsWithoutOurAnnotations(t *testing.T) {
	t.Parallel()
	fake := newFakeCluster()
	fake.objects["someone-elses"] = map[string]any{
		"metadata": map[string]any{
			"name":   "someone-elses",
			"labels": map[string]any{OwnerLabel: "true", ServiceLabel: ServiceLabelValue},
		},
		"data": map[string]any{"v1": base64.StdEncoding.EncodeToString([]byte("nope"))},
	}
	s := NewStore(NewKubeStore(fake, "cloudburrow", "test"))

	list, err := s.ListSecrets("demo")
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("an unannotated object was reported as a secret: %+v", list)
	}
}

func TestKubeStoreRejectsForeignKeys(t *testing.T) {
	t.Parallel()
	k := NewKubeStore(newFakeCluster(), "cloudburrow", "test")

	for _, key := range []string{"", "other/thing", "secret/only-two", "version/p/s/notanumber"} {
		if _, err := k.Get(key); err == nil {
			t.Errorf("Get(%q) accepted a key that is not ours", key)
		}
		if err := k.Put(key, []byte("{}")); err == nil {
			t.Errorf("Put(%q) accepted a key that is not ours", key)
		}
	}
}

// A cluster error must surface, not be mistaken for "not found" — which the
// store would report as a missing secret and a caller would read as a bug in
// their own code.
func TestClusterErrorsAreNotMistakenForAbsence(t *testing.T) {
	t.Parallel()
	fake := newFakeCluster()
	fake.failGet = true
	k := NewKubeStore(fake, "cloudburrow", "test")

	if _, err := k.Get("secret/demo/k"); err == nil {
		t.Fatal("a cluster error was reported as success")
	}
}

// --- the Cloud Run resolver -------------------------------------------

func TestResolveSecretRefPointsAtTheKubernetesObject(t *testing.T) {
	t.Parallel()
	s, _ := newKubeBackedStore(t)
	seedKube(t, s, "demo", "api-key", "one")
	if _, err := s.AddVersion("demo", "api-key", []byte("two")); err != nil {
		t.Fatal(err)
	}

	name, key, err := s.ResolveSecretRef("demo", "api-key", "latest")
	if err != nil {
		t.Fatalf("ResolveSecretRef: %v", err)
	}
	if name != KubernetesSecretName("demo", "api-key") {
		t.Errorf("secret name = %q", name)
	}
	// latest must become a concrete key: Kubernetes has no "latest".
	if key != VersionKey(2) {
		t.Errorf("data key = %q, want %q", key, VersionKey(2))
	}

	// An explicit version is honoured.
	_, key, err = s.ResolveSecretRef("demo", "api-key", "1")
	if err != nil {
		t.Fatal(err)
	}
	if key != VersionKey(1) {
		t.Errorf("data key = %q, want %q", key, VersionKey(1))
	}

	// An empty version means latest, as Cloud Run defines it.
	_, key, err = s.ResolveSecretRef("demo", "api-key", "")
	if err != nil {
		t.Fatal(err)
	}
	if key != VersionKey(2) {
		t.Errorf("empty version resolved to %q, want latest", key)
	}
}

func TestResolveSecretRefAcceptsAFullResourceName(t *testing.T) {
	t.Parallel()
	s, _ := newKubeBackedStore(t)
	seedKube(t, s, "elsewhere", "k", "x")

	// The full name wins over the service's own project, because it says
	// which project it means.
	name, _, err := s.ResolveSecretRef("service-project", "projects/elsewhere/secrets/k", "latest")
	if err != nil {
		t.Fatalf("ResolveSecretRef: %v", err)
	}
	if name != KubernetesSecretName("elsewhere", "k") {
		t.Errorf("resolved to %q, want the project the reference named", name)
	}
}

// A pod given a reference to a disabled version fails to start with
// Kubernetes complaining about a missing key, which points nowhere near the
// cause. It must be refused at deployment instead.
func TestResolveSecretRefRefusesAnInaccessibleVersion(t *testing.T) {
	t.Parallel()
	s, _ := newKubeBackedStore(t)
	seedKube(t, s, "demo", "k", "x")

	if _, err := s.SetVersionState("demo", "k", "1", StateDisabled); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.ResolveSecretRef("demo", "k", "1")
	wantCode(t, err, codes.FailedPrecondition)

	if _, err := s.SetVersionState("demo", "k", "1", StateEnabled); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DestroyVersion("demo", "k", "1"); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.ResolveSecretRef("demo", "k", "1")
	wantCode(t, err, codes.FailedPrecondition)
}

func TestResolveSecretRefReportsMissingSecrets(t *testing.T) {
	t.Parallel()
	s, _ := newKubeBackedStore(t)

	_, _, err := s.ResolveSecretRef("demo", "nope", "latest")
	wantCode(t, err, codes.NotFound)

	_, _, err = s.ResolveSecretRef("", "bare-id", "latest")
	wantCode(t, err, codes.InvalidArgument)
}

func seedKube(t *testing.T, s *Store, project, id string, payloads ...string) {
	t.Helper()
	if _, err := s.CreateSecret(project, id, nil, nil, ""); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	for _, p := range payloads {
		if _, err := s.AddVersion(project, id, []byte(p)); err != nil {
			t.Fatalf("AddVersion: %v", err)
		}
	}
}

// A client-side apply records the whole object, `data` included, in the
// last-applied-configuration annotation — putting every payload back in an
// annotation, exactly where splitting it out was meant to keep it from being.
func TestApplyIsServerSideSoPayloadsStayOutOfAnnotations(t *testing.T) {
	t.Parallel()
	s, fake := newKubeBackedStore(t)
	seedKube(t, s, "demo", "k", "x")

	var applies int
	for _, call := range fake.calls {
		if !strings.Contains(call, "apply") {
			continue
		}
		applies++
		if !strings.Contains(call, "--server-side") {
			t.Errorf("apply is client-side, which copies every payload into "+
				"last-applied-configuration: %q", call)
		}
		if !strings.Contains(call, "--force-conflicts") {
			t.Errorf("apply would fail on a field conflict rather than taking "+
				"ownership, leaving the API reporting a write the cluster did not take: %q", call)
		}
	}
	if applies == 0 {
		t.Fatal("nothing was applied")
	}
}
