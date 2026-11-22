package marketplace

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

func helloChart() *chart.Chart {
	return &chart.Chart{
		Metadata: &chart.Metadata{APIVersion: "v2", Name: "hello", Version: "0.1.0", Description: "hi"},
		Values:   map[string]any{"replicaCount": 1},
		Schema:   []byte(`{"properties":{"replicaCount":{"type":"integer"}}}`),
		Templates: []*chart.File{
			{Name: "templates/deployment.yaml", Data: []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: {{ .Release.Name }}\nspec:\n  replicas: {{ .Values.replicaCount }}\n")},
			{Name: "templates/service.yaml", Data: []byte("apiVersion: v1\nkind: Service\nmetadata:\n  name: {{ .Release.Name }}\n")},
		},
	}
}

// TestKubeHelmRelease installs, reads, upgrades and uninstalls a release
// through the Helm SDK over an in-memory release store, and checks the
// Application recorded for it.
func TestKubeHelmRelease(t *testing.T) {
	ctx := context.Background()
	store := storage.Init(driver.NewMemory())
	cfg := &action.Configuration{Releases: store, KubeClient: &kubefake.PrintingKubeClient{Out: io.Discard},
		Capabilities: chartutil.DefaultCapabilities, Log: func(string, ...any) {}}
	var applied string
	f := &fakeKube{answer: func(a string) (string, error) {
		switch {
		case strings.Contains(a, "get namespace shop"):
			return "", notFound(`namespaces "shop"`)
		case strings.Contains(a, "get applications.app.k8s.io web -o name"):
			return "", notFound(`applications.app.k8s.io "web"`)
		case strings.Contains(a, "get applications.app.k8s.io web -o json"):
			return applied, nil
		case strings.Contains(a, "get deployment.apps web -o json"):
			return `{"kind":"Deployment","metadata":{"name":"web"},"spec":{"replicas":2},"status":{"readyReplicas":2,"replicas":2}}`, nil
		case strings.Contains(a, "get service web -o json"):
			return `{"kind":"Service","metadata":{"name":"web"},"spec":{"type":"ClusterIP","clusterIP":"10.0.0.9"}}`, nil
		}
		return "", nil
	}}
	k := Kube{Invoker: f, Helm: func(string, string) (*action.Configuration, error) { return cfg, nil }}
	plan, err := chartPlan(helloChart(), "artifacthub", "bitnami/nginx", "0.1.0", map[string]string{"v.replicaCount": "2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := Target{Cluster: "c", Kubeconfig: "/kc"}
	if err := k.Deploy(ctx, target, AppMeta{Name: "web", Namespace: "shop"}, true, plan); err != nil {
		t.Fatal(err)
	}
	rel, err := store.Last("web")
	if err != nil || rel.Version != 1 || !strings.Contains(rel.Manifest, "replicas: 2") || rel.Namespace != "shop" {
		t.Fatalf("release %+v %v", rel, err)
	}
	for i, c := range f.calls {
		if strings.Contains(c, "apply") && strings.Contains(f.stdins[i], `"kind":"Application"`) {
			applied = f.stdins[i]
		}
		if strings.Contains(c, "kind\":\"Namespace") || strings.Contains(f.stdins[i], `"kind":"Namespace"`) {
			t.Errorf("the namespace is created by kubectl, not by Helm's install: %s", f.stdins[i])
		}
	}
	var app map[string]any
	if err := json.Unmarshal([]byte(applied), &app); err != nil {
		t.Fatalf("no Application applied: %v (%q)", err, applied)
	}
	if !strings.Contains(applied, `"`+AnnotationRelease+`":"web"`) || !strings.Contains(applied, `{"group":"apps","kind":"Deployment"}`) ||
		!strings.Contains(applied, `"app.kubernetes.io/instance":"web"`) || !strings.Contains(applied, `"assemblyPhase":"Succeeded"`) {
		t.Errorf("Application %s", applied)
	}
	a, err := k.Application(ctx, target, "shop", "web")
	if err != nil {
		t.Fatal(err)
	}
	if a.Release != "web" || a.ReleaseStatus != "deployed" || a.Revision != 1 || len(a.Components) != 2 || a.Status != StatusOK {
		t.Errorf("application %+v", a)
	}
	values, version, err := k.ReleaseValues(target, "shop", "web")
	if err != nil || !strings.Contains(values, "replicaCount: 2") || version != "0.1.0" {
		t.Errorf("values %q %q %v", values, version, err)
	}
	plan2, _ := chartPlan(helloChart(), "artifacthub", "bitnami/nginx", "0.1.0", map[string]string{"v.replicaCount": "4"}, nil)
	if err := k.UpgradeApplication(ctx, target, "shop", "web", plan2); err != nil {
		t.Fatal(err)
	}
	if rel, _ := store.Last("web"); rel.Version != 2 || !strings.Contains(rel.Manifest, "replicas: 4") {
		t.Errorf("upgraded release %+v", rel)
	}
	if err := k.DeleteApplication(ctx, target, "shop", "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Last("web"); err == nil {
		t.Error("the release is not uninstalled")
	}
	if last := f.calls[len(f.calls)-1]; !strings.Contains(last, "delete applications.app.k8s.io web") {
		t.Errorf("last call %s", last)
	}
}
