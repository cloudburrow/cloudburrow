package marketplace

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

type fakeKube struct {
	calls  []string
	stdins []string
	answer func(args string) (string, error)
}

func (f *fakeKube) Run(_ context.Context, stdin string, args ...string) (string, error) {
	a := strings.Join(args, " ")
	f.calls = append(f.calls, a)
	f.stdins = append(f.stdins, stdin)
	return f.answer(a)
}

func notFound(what string) error {
	return &k8s.RunError{Err: errors.New("exit status 1"), Stderr: `Error from server (NotFound): ` + what + ` not found`}
}

func TestKubeDeploy(t *testing.T) {
	f := &fakeKube{answer: func(a string) (string, error) {
		switch {
		case strings.Contains(a, "get namespace shop"):
			return "", notFound(`namespaces "shop"`)
		case strings.Contains(a, "get applications.app.k8s.io web-1 -o name"):
			return "", notFound(`applications.app.k8s.io "web-1"`)
		case strings.Contains(a, "jsonpath={.metadata.uid}"):
			return "uid-123", nil
		}
		return "", nil
	}}
	k := Kube{Invoker: f}
	plan, err := (&DockerHub{}).Plan(context.Background(), "library/nginx", "alpine", AppMeta{Name: "web-1", Namespace: "shop"}, map[string]string{"ports": "80"})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Deploy(context.Background(), Target{Cluster: "c", Kubeconfig: "/kc"}, AppMeta{Name: "web-1", Namespace: "shop"}, false, plan); err == nil ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("an absent namespace without Create a namespace: %v", err)
	}
	f.calls, f.stdins = nil, nil
	if err := k.Deploy(context.Background(), Target{Cluster: "c", Kubeconfig: "/kc"}, AppMeta{Name: "web-1", Namespace: "shop"}, true, plan); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	for _, want := range []string{"--kubeconfig /kc", "wait --for condition=established crd/applications.app.k8s.io",
		"create -f -", "patch applications.app.k8s.io web-1 --type merge"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in\n%s", want, joined)
		}
	}
	var crd, app, comps string
	for i, s := range f.stdins {
		switch {
		case strings.Contains(s, `"CustomResourceDefinition"`):
			crd = s
		case strings.Contains(f.calls[i], "create -f -"):
			app = s
		case strings.Contains(s, `"kind":"List"`):
			comps = s
		}
	}
	if !strings.Contains(crd, `"group":"app.k8s.io"`) || !strings.Contains(crd, `"name":"v1beta1"`) {
		t.Errorf("CRD %s", crd)
	}
	if !strings.Contains(app, `"componentKinds":[{"group":"apps","kind":"Deployment"},{"group":"","kind":"Service"}]`) ||
		!strings.Contains(app, `"matchLabels":{"app.kubernetes.io/name":"web-1"}`) || !strings.Contains(app, `"assemblyPhase":"Pending"`) {
		t.Errorf("Application %s", app)
	}
	if !strings.Contains(comps, `"uid":"uid-123"`) || !strings.Contains(comps, `"namespace":"shop"`) {
		t.Errorf("components %s", comps)
	}
	if err := k.Deploy(context.Background(), Target{Kubeconfig: "/kc"}, AppMeta{Name: "Bad_Name", Namespace: "shop"}, true, plan); !errors.Is(err, ErrBadRequest) {
		t.Errorf("bad name: %v", err)
	}
}

func TestKubeApplication(t *testing.T) {
	app := `{"metadata":{"name":"web-1","namespace":"shop","creationTimestamp":"2026-11-22T10:00:00Z",
	"annotations":{"marketplace.cloudburrow.dev/source":"dockerhub","marketplace.cloudburrow.dev/product":"library/nginx"}},
	"spec":{"descriptor":{"type":"nginx","version":"alpine","links":[{"description":"x","url":"javascript:alert(1)"}]},
	"componentKinds":[{"group":"apps","kind":"Deployment"},{"group":"","kind":"Service"}],"assemblyPhase":"Succeeded"}}`
	f := &fakeKube{answer: func(a string) (string, error) {
		switch {
		case strings.Contains(a, "get applications.app.k8s.io web-1 -o json"):
			return app, nil
		case strings.Contains(a, "get deployment.apps -l"):
			return `{"items":[{"metadata":{"name":"web-1"},"spec":{"replicas":2},"status":{"readyReplicas":1}}]}`, nil
		case strings.Contains(a, "get service -l"):
			return `{"items":[{"metadata":{"name":"web-1"},"spec":{"type":"ClusterIP","clusterIP":"10.96.0.9","ports":[{"port":80,"protocol":"TCP"}]}}]}`, nil
		case strings.Contains(a, "get pods -l"):
			return `{"items":[{"metadata":{"name":"web-1-a"},"status":{"phase":"Pending","containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"ImagePullBackOff"}}}]}}]}`, nil
		case strings.Contains(a, "delete"):
			return "", nil
		}
		return "", errors.New("unexpected " + a)
	}}
	k := Kube{Invoker: f}
	a, err := k.Application(context.Background(), Target{Cluster: "c", Kubeconfig: "/kc"}, "shop", "web-1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StatusFailed || !strings.Contains(a.Detail, "ImagePullBackOff") || len(a.Components) != 2 || len(a.Links) != 0 {
		t.Errorf("app %+v", a)
	}
	if a.Components[0].Detail != "1/2 pods ready" || a.Components[0].Status != StatusPending || a.Components[1].Endpoints[0] != "10.96.0.9:80 TCP" {
		t.Errorf("components %+v", a.Components)
	}
	if err := k.DeleteApplication(context.Background(), Target{Kubeconfig: "/kc"}, "shop", "web-1"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	for _, want := range []string{"delete deployment.apps -l app.kubernetes.io/name=web-1 --ignore-not-found", "delete service -l app.kubernetes.io/name=web-1",
		"delete applications.app.k8s.io web-1 --ignore-not-found"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in\n%s", want, joined)
		}
	}
}

func TestKubeApplicationsWithoutCRD(t *testing.T) {
	f := &fakeKube{answer: func(string) (string, error) {
		return "", &k8s.RunError{Err: errors.New("exit status 1"), Stderr: `error: the server doesn't have a resource type "applications"`}
	}}
	apps, err := Kube{Invoker: f}.Applications(context.Background(), Target{Kubeconfig: "/kc"})
	if err != nil || len(apps) != 0 {
		t.Errorf("a cluster with no Application CRD has no applications: %v %v", apps, err)
	}
}
