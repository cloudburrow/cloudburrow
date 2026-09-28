package components

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// preCutoverCluster answers kubectl as a cluster first brought up before
// the builtin storage server (#519) does after its first new `up` began:
// the storage Deployment and Service, and beside them the retired
// storage-internal pair with its pod still running (#780).
func preCutoverCluster(call string) (string, error) {
	switch {
	case strings.Contains(call, "get deployment -l cloudburrow.dev/owned=true,cloudburrow.dev/instance=old -o name"):
		return "deployment.apps/storage\ndeployment.apps/storage-internal\n", nil
	case strings.Contains(call, "get service -l cloudburrow.dev/owned=true -o name"):
		return "service/storage\nservice/storage-internal\n", nil
	case strings.Contains(call, "get pod -l app=storage-internal -o name"):
		return "pod/storage-internal-7d9f-abcde\n", nil
	}
	return "", nil
}

func indexOf(calls []string, sub string) int {
	for i, c := range calls {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

// `up` over a pre-#519 cluster removes storage-internal, names it and why,
// waits for its pod to go, and only then starts the storage server on the
// volume the two shared (#780).
func TestUpOverAPreCutoverClusterRemovesStorageInternalAndStartsStorage(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{respond: preCutoverCluster}
	in := newTestInstaller(r)
	in.Instance = "old"
	var log strings.Builder
	in.Out = &log
	c := &LifecycleComponent{installer: in, services: []config.Service{config.ServiceStorage},
		mode: config.ModePersistent, storageImage: "dev.local/cloudburrow-storage:test",
		timeout: time.Minute, out: &log}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(r.calls, "\n")

	delDeploy := indexOf(r.calls, "-n cloudburrow delete deployment storage-internal --ignore-not-found --wait=true --timeout=60s --cascade=foreground")
	delSvc := indexOf(r.calls, "-n cloudburrow delete service storage-internal --ignore-not-found --wait=true --timeout=60s")
	waitPods := indexOf(r.calls, "-n cloudburrow wait --for=delete pod -l app=storage-internal --timeout=60s")
	rollout := indexOf(r.calls, "-n cloudburrow rollout status deployment/storage --watch=true")
	applyStorage := -1
	for i, stdin := range r.stdins {
		if strings.Contains(stdin, "name: storage\n") && strings.Contains(stdin, "kind: Deployment") {
			applyStorage = i
		}
	}
	if delDeploy < 0 || delSvc < 0 || waitPods < 0 || applyStorage < 0 || rollout < 0 {
		t.Fatalf("delete deployment %d, delete service %d, wait %d, apply storage %d, rollout %d:\n%s",
			delDeploy, delSvc, waitPods, applyStorage, rollout, calls)
	}
	if !(delDeploy < waitPods && waitPods < applyStorage && delSvc < applyStorage && applyStorage < rollout) {
		t.Errorf("storage-internal must be gone before storage is applied:\n%s", calls)
	}
	for _, call := range r.calls {
		if strings.Contains(call, " delete ") && !strings.Contains(call, "storage-internal") {
			t.Errorf("deleted something other than storage-internal: %s", call)
		}
	}
	for _, want := range []string{
		"removing deployment storage-internal: the in-cluster Cloud Storage server CloudBurrow ran before its builtin server (#519)",
		"removing service storage-internal:",
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("up's output does not say %q:\n%s", want, log.String())
		}
	}
}

// Only objects carrying CloudBurrow's labels are removed: kubectl is asked
// by selector, and an object named storage-internal that the selector
// leaves out, or one with a longer name, is never touched. A cluster with
// nothing retired gets no delete at all.
func TestRemoveRetiredTouchesOnlyWhatCloudBurrowLabelled(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{respond: func(call string) (string, error) {
		if strings.Contains(call, "get deployment") {
			return "deployment.apps/storage\ndeployment.apps/storage-internal-mine\n", nil
		}
		return "", nil
	}}
	in := newTestInstaller(r)
	in.Instance = "old"
	removed, err := in.RemoveRetired(context.Background(), time.Minute)
	if err != nil || len(removed) != 0 {
		t.Fatalf("RemoveRetired = %v, %v", removed, err)
	}
	for _, call := range r.calls {
		if strings.Contains(call, " delete ") {
			t.Errorf("deleted on a cluster with nothing retired: %s", call)
		}
	}
	for _, want := range []string{"-l cloudburrow.dev/owned=true,cloudburrow.dev/instance=old", "get service -l cloudburrow.dev/owned=true"} {
		if indexOf(r.calls, want) < 0 {
			t.Errorf("no lookup by %q:\n%s", want, strings.Join(r.calls, "\n"))
		}
	}
}

// A retired Deployment that cannot be removed stops `up` with its name
// before any backend starts on the volume it still holds.
func TestAFailedRemovalStopsUpBeforeStorageStarts(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{respond: func(call string) (string, error) {
		if strings.Contains(call, "delete deployment storage-internal") {
			return "", errors.New("kubectl: exit status 1: Error from server (Forbidden): deployments.apps is forbidden")
		}
		return preCutoverCluster(call)
	}}
	in := newTestInstaller(r)
	in.Instance = "old"
	c := &LifecycleComponent{installer: in, services: []config.Service{config.ServiceStorage},
		mode: config.ModePersistent, timeout: time.Minute, out: &strings.Builder{}}
	err := c.Start(context.Background())
	if !errors.Is(err, ErrInstallFailed) || !strings.Contains(err.Error(), "remove the retired deployment storage-internal") {
		t.Fatalf("Start = %v", err)
	}
	if indexOf(r.calls, "rollout status") >= 0 {
		t.Errorf("storage was started beside storage-internal:\n%s", strings.Join(r.calls, "\n"))
	}
}

// A rollout that fails because the container exits at start carries the
// newest pod's own last lines, so the storage server's refusal of its data
// directory (#780) reaches the developer instead of only "did not become
// ready".
func TestAFailedRolloutCarriesTheNewestPodsLog(t *testing.T) {
	t.Parallel()
	refusal := "cloudburrow-storage: the storage data directory holds buckets this server cannot read: /data holds 1 bucket(s)"
	r := &recordingRunner{respond: func(call string) (string, error) {
		switch {
		case strings.Contains(call, "rollout status"):
			return "", errors.New("kubectl: exit status 1: error: timed out waiting for the condition")
		case strings.Contains(call, "get events"):
			return "Warning BackOff pod/storage-new Back-off restarting failed container", nil
		case strings.Contains(call, "get pods -l app=storage --sort-by=.metadata.creationTimestamp -o name"):
			return "pod/storage-old\npod/storage-new\n", nil
		case strings.Contains(call, "logs pod/storage-new --tail=20"):
			return refusal + "\n", nil
		}
		return "", nil
	}}
	err := newTestInstaller(r).waitDeployment(context.Background(), "cloudburrow", "storage", time.Minute)
	if !errors.Is(err, ErrInstallFailed) {
		t.Fatalf("waitDeployment = %v", err)
	}
	for _, want := range []string{"storage did not become ready within 1m0s", "Back-off restarting", "last lines pod/storage-new logged:", refusal} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not say %q:\n%v", want, err)
		}
	}
}
