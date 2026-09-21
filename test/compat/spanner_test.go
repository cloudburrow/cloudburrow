//go:build compat

package compat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"cloud.google.com/go/spanner"
)

// spannerOpts are the client options for the local emulator. No credentials
// anywhere: the emulator accepts none, and a test that supplied them would be
// proving something about a different deployment.
func spannerOpts(addr string) []option.ClientOption {
	return []option.ClientOption{
		option.WithEndpoint(addr),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	}
}

// spannerDatabase creates an instance, a database and a table, and returns the
// database's resource name.
func spannerDatabase(t *testing.T, ctx context.Context, addr, project, instanceID, dbID, ddl string) string {
	t.Helper()
	opts := spannerOpts(addr)

	instAdmin, err := instance.NewInstanceAdminClient(ctx, opts...)
	if err != nil {
		t.Fatalf("NewInstanceAdminClient: %v", err)
	}
	defer instAdmin.Close()

	op, err := instAdmin.CreateInstance(ctx, &instancepb.CreateInstanceRequest{
		Parent:     "projects/" + project,
		InstanceId: instanceID,
		Instance: &instancepb.Instance{
			Config:      fmt.Sprintf("projects/%s/instanceConfigs/emulator-config", project),
			DisplayName: "CloudBurrow",
			NodeCount:   1,
		},
	})
	// An instance left by an earlier test in the same run is fine to reuse;
	// anything else is a real failure.
	if err != nil && status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateInstance: %v", err)
	}
	if err == nil {
		if _, err := op.Wait(ctx); err != nil {
			t.Fatalf("waiting for the instance: %v", err)
		}
	}

	dbAdmin, err := database.NewDatabaseAdminClient(ctx, opts...)
	if err != nil {
		t.Fatalf("NewDatabaseAdminClient: %v", err)
	}
	defer dbAdmin.Close()

	dbOp, err := dbAdmin.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{
		Parent:          fmt.Sprintf("projects/%s/instances/%s", project, instanceID),
		CreateStatement: "CREATE DATABASE " + dbID,
		ExtraStatements: []string{ddl},
	})
	if err != nil {
		t.Fatalf("CreateDatabase %s: %v", dbID, err)
	}
	if _, err := dbOp.Wait(ctx); err != nil {
		t.Fatalf("waiting for database %s: %v", dbID, err)
	}
	return fmt.Sprintf("projects/%s/instances/%s/databases/%s", project, instanceID, dbID)
}

// TestSpannerReadWriteTransactionIsAtomic covers the criterion that names
// transactions separately from queries.
//
// The previous coverage applied a single mutation, which exercises neither the
// read-modify-write path nor the guarantee that makes Spanner worth using. A
// transaction that commits partially is the failure worth catching, so the
// abort case is asserted as well as the commit.
func TestSpannerReadWriteTransactionIsAtomic(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvSpanner)
	t.Setenv("SPANNER_EMULATOR_HOST", addr)
	ctx := h.Context()

	dbName := spannerDatabase(t, ctx, addr, h.Project(), "cb-txn", "txndb",
		`CREATE TABLE Accounts (Id INT64 NOT NULL, Balance INT64 NOT NULL) PRIMARY KEY (Id)`)

	c, err := spanner.NewClient(ctx, dbName, spannerOpts(addr)...)
	if err != nil {
		t.Fatalf("spanner.NewClient: %v", err)
	}
	defer c.Close()

	if _, err := c.Apply(ctx, []*spanner.Mutation{
		spanner.Insert("Accounts", []string{"Id", "Balance"}, []any{int64(1), int64(100)}),
		spanner.Insert("Accounts", []string{"Id", "Balance"}, []any{int64(2), int64(0)}),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A transfer: read both balances, then write both. This is the shape that
	// needs a transaction rather than two mutations.
	if _, err := c.ReadWriteTransaction(ctx,
		func(ctx context.Context, txn *spanner.ReadWriteTransaction) error {
			var from, to int64
			row, err := txn.ReadRow(ctx, "Accounts", spanner.Key{int64(1)}, []string{"Balance"})
			if err != nil {
				return err
			}
			if err := row.Column(0, &from); err != nil {
				return err
			}
			row, err = txn.ReadRow(ctx, "Accounts", spanner.Key{int64(2)}, []string{"Balance"})
			if err != nil {
				return err
			}
			if err := row.Column(0, &to); err != nil {
				return err
			}
			return txn.BufferWrite([]*spanner.Mutation{
				spanner.Update("Accounts", []string{"Id", "Balance"}, []any{int64(1), from - 40}),
				spanner.Update("Accounts", []string{"Id", "Balance"}, []any{int64(2), to + 40}),
			})
		}); err != nil {
		t.Fatalf("ReadWriteTransaction: %v", err)
	}

	if got, want := spannerBalance(t, ctx, c, 1), int64(60); got != want {
		t.Errorf("account 1 = %d, want %d", got, want)
	}
	if got, want := spannerBalance(t, ctx, c, 2), int64(40); got != want {
		t.Errorf("account 2 = %d, want %d", got, want)
	}

	// A transaction that returns an error must leave nothing behind. Writing
	// one side and failing after it is the bug this catches.
	sentinel := errors.New("deliberate abort")
	_, err = c.ReadWriteTransaction(ctx,
		func(ctx context.Context, txn *spanner.ReadWriteTransaction) error {
			if err := txn.BufferWrite([]*spanner.Mutation{
				spanner.Update("Accounts", []string{"Id", "Balance"}, []any{int64(1), int64(999)}),
			}); err != nil {
				return err
			}
			return sentinel
		})
	if !errors.Is(err, sentinel) {
		t.Fatalf("aborted transaction returned %v, want the sentinel", err)
	}
	if got, want := spannerBalance(t, ctx, c, 1), int64(60); got != want {
		t.Errorf("an aborted transaction was committed: account 1 = %d, want %d", got, want)
	}
}

func spannerBalance(t *testing.T, ctx context.Context, c *spanner.Client, id int64) int64 {
	t.Helper()
	row, err := c.Single().ReadRow(ctx, "Accounts", spanner.Key{id}, []string{"Balance"})
	if err != nil {
		t.Fatalf("ReadRow %d: %v", id, err)
	}
	var v int64
	if err := row.Column(0, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestSpannerDatabasesAreIsolated covers the isolation criterion.
//
// Two databases on the same instance must not see each other's rows. An
// emulator that shared state between them would let a test pass because of
// data another test wrote.
func TestSpannerDatabasesAreIsolated(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvSpanner)
	t.Setenv("SPANNER_EMULATOR_HOST", addr)
	ctx := h.Context()

	const ddl = `CREATE TABLE Items (Id INT64 NOT NULL, Name STRING(MAX)) PRIMARY KEY (Id)`
	a := spannerDatabase(t, ctx, addr, h.Project(), "cb-iso", "isoa", ddl)
	b := spannerDatabase(t, ctx, addr, h.Project(), "cb-iso", "isob", ddl)

	ca, err := spanner.NewClient(ctx, a, spannerOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	cb, err := spanner.NewClient(ctx, b, spannerOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()

	if _, err := ca.Apply(ctx, []*spanner.Mutation{
		spanner.Insert("Items", []string{"Id", "Name"}, []any{int64(1), "only in A"}),
	}); err != nil {
		t.Fatalf("write to A: %v", err)
	}

	if _, err := cb.Single().ReadRow(ctx, "Items", spanner.Key{int64(1)}, []string{"Name"}); err == nil {
		t.Error("a row written to database A is readable from database B")
	} else if status.Code(err) != codes.NotFound {
		t.Errorf("reading B returned %v, want NotFound", err)
	}
}

// TestSpannerStateDoesNotSurviveARestart measures durability instead of
// citing it.
//
// The documentation says the emulator is in-memory and that CloudBurrow
// provisions no volume for it. That was taken from Google's description
// rather than observed, and #37 asks for actual durability to be documented
// rather than assumed. This restarts the component and looks.
//
// It is skipped unless the kubeconfig is available, because restarting the
// workload is the only way to ask the question.
func TestSpannerStateDoesNotSurviveARestart(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvSpanner)
	kubeconfig := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_KUBECONFIG"))
	if kubeconfig == "" {
		t.Skip("CLOUDBURROW_TEST_KUBECONFIG is not set; a restart cannot be performed")
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required to restart the component")
	}
	t.Setenv("SPANNER_EMULATOR_HOST", addr)
	ctx := h.Context()

	dbName := spannerDatabase(t, ctx, addr, h.Project(), "cb-durab", "durabdb",
		`CREATE TABLE Survivors (Id INT64 NOT NULL, Name STRING(MAX)) PRIMARY KEY (Id)`)

	c, err := spanner.NewClient(ctx, dbName, spannerOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(ctx, []*spanner.Mutation{
		spanner.Insert("Survivors", []string{"Id", "Name"}, []any{int64(1), "before restart"}),
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	c.Close()

	restartWorkload(t, kubeconfig, "spanner")

	// The endpoint must come back before anything can be concluded. Without
	// this the test passes whenever the address is unreachable, which is not
	// evidence about durability at all — it is evidence the tunnel is broken.
	// That is exactly how this test first "passed".
	waitForEndpoint(t, addr, 2*time.Minute)

	c2, err := spanner.NewClient(ctx, dbName, spannerOpts(addr)...)
	if err != nil {
		t.Fatalf("client after restart: %v", err)
	}
	defer c2.Close()

	readCtx, cancelRead := context.WithTimeout(ctx, 30*time.Second)
	defer cancelRead()
	row, err := c2.Single().ReadRow(readCtx, "Survivors", spanner.Key{int64(1)}, []string{"Name"})
	if err == nil {
		var name string
		_ = row.Column(0, &name)
		t.Errorf("the row %q survived a restart: the component is persisting state, "+
			"and the documentation says it does not", name)
		return
	}
	// NotFound means the database or the row is gone, which is the claim.
	// Anything else means the emulator is unhealthy and the test proved
	// nothing, so it is not accepted as evidence.
	if code := status.Code(err); code != codes.NotFound {
		t.Fatalf("after the restart the read failed with %v (%s), which is not evidence "+
			"about durability — the emulator is not answering normally", err, code)
	}
	t.Logf("after the restart the data is gone: %v", err)
	t.Log("DURABILITY: none, measured — the emulator holds state in memory only")
}

// TestHostEndpointSurvivesABackendRestart is the regression test for a defect
// this issue's restart criterion exposed.
//
// `kubectl port-forward` binds one pod and exits when that pod goes away. The
// forwarder started it once and watched nothing, so a crash, an OOM kill, an
// eviction or a rollout left the advertised host endpoint refused for the life
// of the instance — while the pod was Running, the service existed, and the
// startup banner still printed the dead address. An application would see
// connection refused forever against a cluster that looked healthy.
func TestHostEndpointSurvivesABackendRestart(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvSpanner)
	kubeconfig := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_KUBECONFIG"))
	if kubeconfig == "" {
		t.Skip("CLOUDBURROW_TEST_KUBECONFIG is not set; a restart cannot be performed")
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required to restart the component")
	}

	restartWorkload(t, kubeconfig, "spanner")
	waitForEndpoint(t, addr, 2*time.Minute)

	// Reachable is necessary but not sufficient: the tunnel must carry a real
	// request, not merely accept a connection.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	instAdmin, err := instance.NewInstanceAdminClient(ctx, spannerOpts(addr)...)
	if err != nil {
		t.Fatalf("client after restart: %v", err)
	}
	defer instAdmin.Close()
	if _, err := instAdmin.GetInstance(ctx, &instancepb.GetInstanceRequest{
		Name: fmt.Sprintf("projects/%s/instances/does-not-exist", h.Project()),
	}); status.Code(err) != codes.NotFound {
		t.Fatalf("the restored tunnel does not carry requests: %v", err)
	}
}

// waitForEndpoint blocks until the address accepts a connection.
//
// It fails rather than skips on timeout. A test that quietly tolerates a dead
// endpoint reports success for a broken instance.
func waitForEndpoint(t *testing.T, addr string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	var lastErr error
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	t.Fatalf("%s did not accept a connection within %s after the restart: %v — "+
		"the port-forward did not recover", addr, within, lastErr)
}

// restartWorkload deletes the component's pod and waits for a new one.
//
// Deleting the pod rather than the deployment keeps ownership with whatever
// created it: this test restarts a workload, it does not take over managing
// one.
func restartWorkload(t *testing.T, kubeconfig, name string) {
	t.Helper()
	ns := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_NAMESPACE"))
	if ns == "" {
		ns = "cloudburrow"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", kubeconfig, "-n", ns}, args...)...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return out.String(), err
	}

	if out, err := run("delete", "pod", "-l", "app="+name, "--wait=true"); err != nil {
		t.Skipf("could not restart %s (%v): %s", name, err, out)
	}
	if out, err := run("rollout", "status", "deployment/"+name, "--timeout=2m"); err != nil {
		t.Fatalf("the %s deployment did not come back: %v\n%s", name, err, out)
	}
}

// spannerProbeImage is the fixture's tag. The dev.local/ prefix keeps Knative
// and the kubelet from trying to resolve it against a registry.
const spannerProbeImage = "dev.local/spanner-probe:test"

// TestSpannerFromInsideAPod is the half of #37's first criterion that was
// missing: "from host and pod".
//
// Those are different claims. The host reaches the emulator through a
// port-forward on 127.0.0.1; a pod reaches it through cluster DNS and the
// service network, which is the path an actual application takes. A host-only
// test leaves that path unverified, and a service name that does not resolve
// in-cluster would not show up in it.
func TestSpannerFromInsideAPod(t *testing.T) {
	h := New(t)
	// The host endpoint is required so the test skips consistently with the
	// rest of the Spanner coverage when the service is not enabled.
	_ = h.Endpoint(EnvSpanner)

	kubeconfig := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_KUBECONFIG"))
	cluster := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if kubeconfig == "" || cluster == "" {
		t.Skip("CLOUDBURROW_TEST_KUBECONFIG and CLOUDBURROW_TEST_CLUSTER are required to run a pod")
	}
	for _, bin := range []string{"docker", "kind", "kubectl", "go"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is required to build and run the pod fixture", bin)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildSpannerProbe(t, ctx, cluster)

	ns := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_NAMESPACE"))
	if ns == "" {
		ns = "cloudburrow"
	}
	// The in-cluster address, which is what makes this a different test from
	// the host one.
	inCluster := fmt.Sprintf("spanner.%s.svc.cluster.local:9010", ns)

	job := "spanner-probe"
	kubectl := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "kubectl",
			append([]string{"--kubeconfig", kubeconfig, "-n", ns}, args...)...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return out.String(), err
	}
	// A leftover job from an earlier run would make this report someone
	// else's result.
	_, _ = kubectl("delete", "job", job, "--ignore-not-found", "--wait=true")
	t.Cleanup(func() {
		cleanCtx, cancelClean := context.WithTimeout(context.Background(), time.Minute)
		defer cancelClean()
		cmd := exec.CommandContext(cleanCtx, "kubectl", "--kubeconfig", kubeconfig, "-n", ns,
			"delete", "job", job, "--ignore-not-found", "--wait=false")
		_ = cmd.Run()
	})

	manifest := fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata:
  name: %s
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
      - name: probe
        image: %s
        imagePullPolicy: Never
        env:
        - name: SPANNER_EMULATOR_HOST
          value: %q
        - name: SPANNER_PROJECT
          value: %q
        - name: SPANNER_INSTANCE
          value: "cb-pod"
`, job, spannerProbeImage, inCluster, h.Project())

	apply := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", ns,
		"apply", "--server-side", "-f", "-")
	apply.Stdin = strings.NewReader(manifest)
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("apply the probe job: %v\n%s", err, out)
	}

	waitOut, waitErr := kubectl("wait", "--for=condition=complete", "job/"+job, "--timeout=5m")
	logs, _ := kubectl("logs", "job/"+job)
	if waitErr != nil {
		failed, _ := kubectl("get", "job", job, "-o",
			"jsonpath={.status.conditions[*].type}={.status.conditions[*].message}")
		t.Fatalf("the probe job did not complete: %v\n%s\njob: %s\nlogs:\n%s",
			waitErr, waitOut, failed, logs)
	}

	if !strings.Contains(logs, "SPANNER POD PROBE: OK") {
		t.Fatalf("the probe did not report success:\n%s", logs)
	}
	// The address in the log is the one the pod actually used, which is what
	// makes this evidence rather than a claim about configuration.
	if !strings.Contains(logs, inCluster) {
		t.Errorf("the probe did not report the in-cluster endpoint %q:\n%s", inCluster, logs)
	}
	t.Logf("%s", strings.TrimSpace(logs))
}

// buildSpannerProbe compiles the fixture for the cluster's platform and loads
// it into CloudBurrow's own cluster.
//
// The binary is built on the host with this repository's pinned module
// versions, so the image needs no network and the SDK exercised in the pod is
// the same one the host tests use. Nothing is pushed anywhere and no image
// CloudBurrow did not create is touched.
func buildSpannerProbe(t *testing.T, ctx context.Context, cluster string) {
	t.Helper()
	root := moduleRoot(t)
	dir := root + "/testdata/spannerprobe"

	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", dir+"/spannerprobe", "./testdata/spannerprobe")
	build.Dir = root
	// kind nodes run the host's architecture; the container is Linux.
	build.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile the spanner probe: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = os.Remove(dir + "/spannerprobe") })

	img := exec.CommandContext(ctx, "docker", "build", "-t", spannerProbeImage, dir)
	if out, err := img.CombinedOutput(); err != nil {
		t.Fatalf("build the spanner probe image: %v\n%s", err, out)
	}
	load := exec.CommandContext(ctx, "kind", "load", "docker-image", spannerProbeImage, "--name", cluster)
	if out, err := load.CombinedOutput(); err != nil {
		t.Fatalf("load the spanner probe into cluster %s: %v\n%s", cluster, err, out)
	}
}
