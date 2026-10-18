package secrets

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// oldLayoutObject is a secret's Kubernetes Secret as KubeStore wrote it
// before #1596: every version's metadata an annotation on it, and every
// payload in its data. It is made from what a Store writes, so the
// metadata is real.
func oldLayoutObject(t *testing.T, project, id string, payloads ...string) map[string]any {
	t.Helper()
	mem := store.NewMemory()
	s := NewStore(mem)
	if _, err := s.CreateSecret(project, id, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	for _, p := range payloads {
		if _, err := s.AddVersion(project, id, []byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DestroyVersion(project, id, "2"); err != nil {
		t.Fatal(err)
	}
	meta, err := mem.Get(secretKey(project, id))
	if err != nil {
		t.Fatal(err)
	}
	annotations := map[string]any{
		SecretAnnotation:                 string(meta),
		SecretIDAnnotation:               id,
		"cloudburrow.dev/secret-project": project,
	}
	data := map[string]any{}
	for n := 1; n <= len(payloads); n++ {
		raw, err := mem.Get(versionKey(project, id, n))
		if err != nil {
			t.Fatal(err)
		}
		var v Version
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		if len(v.Payload) > 0 {
			data[VersionKey(n)] = base64.StdEncoding.EncodeToString(v.Payload)
		}
		v.Payload = nil
		b, _ := json.Marshal(v)
		annotations[VersionAnnotationPrefix+VersionKey(n)[1:]] = string(b)
	}
	return map[string]any{
		"metadata": map[string]any{
			"name":        KubernetesSecretName(project, id),
			"labels":      map[string]any{OwnerLabel: "true", ServiceLabel: ServiceLabelValue},
			"annotations": annotations,
		},
		"data": data,
	}
}

// A secret stored before #1596 is moved to a Secret per version when the
// instance starts with the new code: its versions read back as they were,
// the annotations leave its own Secret, and the payloads stay there, as a
// revision deployed earlier references them. A second start finds nothing
// left to migrate.
func TestOldLayoutIsMigratedToASecretPerVersion(t *testing.T) {
	t.Parallel()
	fake := newFakeCluster()
	fake.objects[KubernetesSecretName("demo", "old")] = oldLayoutObject(t, "demo", "old", "one", "two", "three")

	k := NewKubeStore(k8s.NewWith(fake, "", "", "cloudburrow"), "test")
	if err := k.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := NewStore(k)

	for n, want := range map[int]string{1: "one", 3: "three"} {
		obj := fake.version(t, "demo", "old", n)
		data, _ := obj["data"].(map[string]any)
		raw, _ := base64.StdEncoding.DecodeString(data[VersionKey(n)].(string))
		if string(raw) != want {
			t.Errorf("version %d's Secret holds %q, want %q", n, raw, want)
		}
	}
	if data, _ := fake.version(t, "demo", "old", 2)["data"].(map[string]any); len(data) != 0 {
		t.Errorf("destroyed version 2 has data %v", data)
	}
	head := fake.object(t, "demo", "old")
	annotations, _ := head["metadata"].(map[string]any)["annotations"].(map[string]any)
	for name := range annotations {
		if strings.HasPrefix(name, VersionAnnotationPrefix) {
			t.Errorf("annotation %s is still on the secret's Secret", name)
		}
	}
	if _, ok := annotations[SecretAnnotation]; !ok {
		t.Error("the secret's metadata was lost")
	}
	legacy, _ := head["data"].(map[string]any)
	if _, ok := legacy[VersionKey(1)]; !ok {
		t.Errorf("the payload an earlier revision mounts was removed: %v", legacy)
	}

	v, err := s.AccessVersion("demo", "old", LatestAlias)
	if err != nil || string(v.Payload) != "three" {
		t.Fatalf("latest = %q, %v", v.Payload, err)
	}
	versions, err := s.ListVersions("demo", "old")
	if err != nil || len(versions) != 3 {
		t.Fatalf("ListVersions = %d, %v", len(versions), err)
	}
	// The next version is numbered after the migrated ones.
	added, err := s.AddVersion("demo", "old", []byte("four"))
	if err != nil || added.Number != 4 {
		t.Fatalf("AddVersion = %d, %v", added.Number, err)
	}

	// Destroying a migrated version removes the payload kept for old
	// revisions too.
	if _, err := s.DestroyVersion("demo", "old", "1"); err != nil {
		t.Fatal(err)
	}
	legacy, _ = fake.object(t, "demo", "old")["data"].(map[string]any)
	if _, ok := legacy[VersionKey(1)]; ok {
		t.Error("the destroyed version's payload survived on the secret's Secret")
	}

	// A restart lists once and writes nothing.
	fake.calls = nil
	again := NewStore(NewKubeStore(k8s.NewWith(fake, "", "", "cloudburrow"), "test"))
	if v, err := again.AccessVersion("demo", "old", "3"); err != nil || string(v.Payload) != "three" {
		t.Fatalf("after a restart, version 3 = %q, %v", v.Payload, err)
	}
	if len(fake.calls) != 1 {
		t.Errorf("a restart ran %q, want one list", fake.calls)
	}

	// Deleting the secret deletes every Secret of it.
	if err := again.DeleteSecret("demo", "old"); err != nil {
		t.Fatal(err)
	}
	if len(fake.objects) != 0 {
		t.Errorf("deletion left %v", fake.names())
	}
}

// A migration cut short (a version's Secret written, the annotations not
// yet removed) is finished by the next start, and the version's own Secret
// wins over the annotation.
func TestMigrationCutShortIsFinished(t *testing.T) {
	t.Parallel()
	fake := newFakeCluster()
	fake.objects[KubernetesSecretName("demo", "half")] = oldLayoutObject(t, "demo", "half", "one", "two")
	k := NewKubeStore(k8s.NewWith(fake, "", "", "cloudburrow"), "test")
	if err := k.Load(); err != nil {
		t.Fatal(err)
	}
	// Put the old object back, as though the head rewrite never happened,
	// and change version 1's own Secret, which must win.
	fake.objects[KubernetesSecretName("demo", "half")] = oldLayoutObject(t, "demo", "half", "one", "two")
	obj := fake.version(t, "demo", "half", 1)
	obj["data"] = map[string]any{VersionKey(1): base64.StdEncoding.EncodeToString([]byte("newer"))}

	s := NewStore(NewKubeStore(k8s.NewWith(fake, "", "", "cloudburrow"), "test"))
	if v, err := s.AccessVersion("demo", "half", "1"); err != nil || string(v.Payload) != "newer" {
		t.Fatalf("version 1 = %q, %v", v.Payload, err)
	}
	annotations, _ := fake.object(t, "demo", "half")["metadata"].(map[string]any)["annotations"].(map[string]any)
	for name := range annotations {
		if strings.HasPrefix(name, VersionAnnotationPrefix) {
			t.Errorf("annotation %s survived the second start", name)
		}
	}
}

// #1596: a secret's versions are not bounded by Kubernetes' 256 KiB of
// annotations on one object, and a call costs the same kubectl whatever
// the count: none for a read, one apply for a write.
func TestManyVersionsStaySmallAndFlat(t *testing.T) {
	t.Parallel()
	s, fake := newKubeBackedStore(t)
	seedKube(t, s, "demo", "many")
	for i := 0; i < 1500; i++ {
		fake.calls = nil
		if _, err := s.AddVersion("demo", "many", []byte("twelve bytes")); err != nil {
			t.Fatalf("AddVersion %d: %v", i+1, err)
		}
		if i > 0 && len(fake.calls) > 2 {
			t.Fatalf("AddVersion %d ran %q", i+1, fake.calls)
		}
	}
	for name, obj := range fake.objects {
		b, _ := json.Marshal(obj["metadata"].(map[string]any)["annotations"])
		if len(b) > 16*1024 {
			t.Errorf("%s has %d bytes of annotations", name, len(b))
		}
	}
	fake.calls = nil
	for _, ref := range []string{"1", "1500", LatestAlias} {
		if _, err := s.AccessVersion("demo", "many", ref); err != nil {
			t.Fatalf("AccessVersion %s: %v", ref, err)
		}
	}
	if len(fake.calls) != 0 {
		t.Errorf("reads ran %q", fake.calls)
	}
}
