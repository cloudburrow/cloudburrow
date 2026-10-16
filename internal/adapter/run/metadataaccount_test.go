package run

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// aliasKube records applies and answers a get of one Service.
type aliasKube struct {
	mu      sync.Mutex
	applied []string
	service map[string]string
}

func (k *aliasKube) Run(_ context.Context, stdin string, args ...string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	args = args[4:]
	switch {
	case args[0] == "apply":
		k.applied = append(k.applied, stdin)
		return "", nil
	case args[0] == "get" && args[1] == "service":
		if j, ok := k.service[args[2]]; ok {
			return j, nil
		}
	}
	return "", errors.New("NotFound")
}

// A workload's GCE_METADATA_HOST names its account's alias of the metadata
// server, on the same port, and the alias is an ExternalName Service to the
// host the instance injects, recording the account (#1388). A workload with
// no serviceAccount runs as the project's Compute Engine default account.
func TestWorkloadMetadataHostIsItsAccountsAlias(t *testing.T) {
	kube := &aliasKube{}
	s := NewServer(&Knative{Kube: k8s.NewWith(kube, "k", "", "cloudburrow-run")}, "inst", time.Second).
		WithEnvironment(func() map[string]string {
			return map[string]string{"GCE_METADATA_HOST": "cloudburrow-host.cloudburrow.svc.cluster.local:9004"}
		})
	const sa = "runner@my-project.iam.gserviceaccount.com"
	host := func(sa string) string {
		for _, e := range s.injectedFor("my-project", sa) {
			if e.name == "GCE_METADATA_HOST" {
				return e.value
			}
		}
		return ""
	}
	if got, want := host(sa), metadataAliasLabel(sa)+".cloudburrow-run.svc.cluster.local:9004"; got != want {
		t.Errorf("GCE_METADATA_HOST = %q, want %q", got, want)
	}
	if got := DefaultComputeAccount("my-project"); !strings.HasSuffix(got, "-compute@developer.gserviceaccount.com") {
		t.Errorf("DefaultComputeAccount = %q", got)
	}
	if host("") != metadataAliasLabel(DefaultComputeAccount("my-project"))+".cloudburrow-run.svc.cluster.local:9004" {
		t.Errorf("no serviceAccount: GCE_METADATA_HOST = %q, want the default compute account's", host(""))
	}
	if len(metadataAliasLabel(strings.Repeat("a", 30)+"@"+strings.Repeat("b", 30)+".iam.gserviceaccount.com")) > 63 {
		t.Error("an alias label is longer than a DNS label")
	}

	ctx := context.Background()
	for range 2 {
		if err := s.ensureWorkloadMetadata(ctx, "my-project", sa); err != nil {
			t.Fatal(err)
		}
	}
	if len(kube.applied) != 2 {
		t.Fatalf("applied %d aliases, want one per deploy", len(kube.applied))
	}
	for _, want := range []string{"kind: Service", "type: ExternalName", "externalName: cloudburrow-host.cloudburrow.svc.cluster.local",
		"name: " + metadataAliasLabel(sa), `run.cloudburrow.dev/service-account: "` + sa + `"`} {
		if !strings.Contains(kube.applied[0], want) {
			t.Errorf("the alias lacks %q:\n%s", want, kube.applied[0])
		}
	}
	if got, ok := s.MetadataAccount(metadataAliasLabel(sa)); !ok || got != sa {
		t.Errorf("MetadataAccount = %q, %v; want %s", got, ok, sa)
	}

	// After a restart the alias is read back from its Service.
	other := "other@my-project.iam.gserviceaccount.com"
	kube.service = map[string]string{metadataAliasLabel(other): `{"metadata":{"labels":{"run.cloudburrow.dev/metadata-alias":"true"},` +
		`"annotations":{"run.cloudburrow.dev/service-account":"` + other + `"}}}`}
	if got, ok := s.MetadataAccount(metadataAliasLabel(other)); !ok || got != other {
		t.Errorf("MetadataAccount from the Service = %q, %v; want %s", got, ok, other)
	}
	if _, ok := s.MetadataAccount("cloudburrow-sa-unknown"); ok {
		t.Error("an unknown label resolved")
	}
}

// A worker pool's pods are given their template's account's metadata host.
func TestWorkerPoolPodsNameTheirAccount(t *testing.T) {
	s := NewServer(&Knative{}, "inst", time.Second).WithEnvironment(func() map[string]string {
		return map[string]string{"GCE_METADATA_HOST": "cloudburrow-host.cloudburrow.svc.cluster.local:9004"}
	})
	const sa = "pool@my-project.iam.gserviceaccount.com"
	w := workload{kind: "workerPool", id: "wp", name: "projects/my-project/locations/us-central1/workerPools/wp"}
	spec := &runpb.WorkerPool{Name: w.name, LatestCreatedRevision: w.name + "/revisions/wp-00001-abc",
		Template: &runpb.WorkerPoolRevisionTemplate{ServiceAccount: sa, Containers: []*runpb.Container{{Image: "example.com/img:v1"}}}}
	_, dep, err := s.renderWorkerPool(w, spec, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dep, metadataAliasLabel(sa)+".") {
		t.Errorf("the Deployment does not name %s's metadata host:\n%s", sa, dep)
	}
}
