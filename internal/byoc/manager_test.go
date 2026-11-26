package byoc_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/byoc/byoctest"
	"github.com/cloudburrow/cloudburrow/internal/byoc/keychain"
)

type recordingEngine struct {
	mu        sync.Mutex
	connected []byoc.RemoteTarget
	closed    []string
}

func (e *recordingEngine) Connect(_ context.Context, t byoc.RemoteTarget) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.connected = append(e.connected, t)
	return nil
}
func (e *recordingEngine) Status(context.Context, string) (byoc.RemoteStatus, error) {
	return byoc.RemoteStatus{Connected: true}, nil
}
func (e *recordingEngine) Reconnect(context.Context, string) error { return nil }
func (e *recordingEngine) Close(i string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = append(e.closed, i)
	return nil
}
func (e *recordingEngine) Events(string) <-chan byoc.RemoteEvent { return nil }

type fixture struct {
	m      *byoc.Manager
	aws    *byoctest.Cloud
	azure  *byoctest.Cloud
	keys   *keychain.Memory
	engine *recordingEngine
	dir    string
	opened []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{aws: byoctest.New(byoc.AWS), azure: byoctest.New(byoc.Azure), keys: keychain.NewMemory(),
		engine: &recordingEngine{}, dir: t.TempDir()}
	m, err := byoc.NewManager(byoc.Options{Dir: f.dir, Keychain: f.keys, Drivers: []byoc.Driver{f.aws, f.azure}, Engine: f.engine,
		IPLookup:    func(context.Context) (string, error) { return "203.0.113.9", nil },
		OpenBrowser: func(u string) error { f.opened = append(f.opened, u); return nil },
		Location:    func() (float64, float64, bool) { return 51.5, -0.1, true }})
	if err != nil {
		t.Fatal(err)
	}
	f.m = m
	return f
}

func (f *fixture) connectAWS(t *testing.T) byoc.Connection {
	t.Helper()
	ctx := context.Background()
	st, err := f.m.BeginSignIn(ctx, byoc.AWS, byoc.SignInOptions{StartURL: "https://example.awsapps.com/start", Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "waiting" || st.Prompt.UserCode != "ABCD-EFGH" || len(f.opened) != 1 {
		t.Fatalf("sign-in began %+v, opened %v", st, f.opened)
	}
	f.aws.Approve()
	st, err = f.m.WaitSignIn(ctx, st.ID)
	if err != nil || st.State != "choose" || len(st.Choices) != 2 {
		t.Fatalf("after approval %+v, %v", st, err)
	}
	if _, err := f.m.ChooseAccount(ctx, st.ID, "not-offered"); err == nil {
		t.Fatal("a choice the sign-in did not offer was accepted")
	}
	conn, err := f.m.ChooseAccount(ctx, st.ID, "111122223333")
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestSignInKeepsTheSecretInTheKeychainOnly(t *testing.T) {
	f := newFixture(t)
	conn := f.connectAWS(t)
	if conn.ID != "aws-111122223333" {
		t.Fatalf("connection %+v", conn)
	}
	if _, err := f.keys.Get(byoc.SecretName(conn.ID)); err != nil {
		t.Fatalf("the keychain lacks the secret: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(f.dir, "byoc.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "t-111122223333") {
		t.Fatal("the credentials were written to the state file")
	}
	if fi, _ := os.Stat(filepath.Join(f.dir, "byoc.json")); fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("the state file is %v, want owner-only", fi.Mode().Perm())
	}
	// Disconnect removes the keychain item and the record.
	if err := f.m.Disconnect(conn.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.keys.Get(byoc.SecretName(conn.ID)); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("Disconnect left the secret: %v", err)
	}
	if cs, _ := f.m.Connections(); len(cs) != 0 {
		t.Fatalf("connections after Disconnect: %v", cs)
	}
	if err := f.m.Disconnect(conn.ID, false); !errors.Is(err, byoc.ErrNotFound) {
		t.Fatalf("a second Disconnect = %v", err)
	}
}

func TestAccessKeyFallback(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.m.ConnectKey(ctx, byoc.AWS, byoc.AccessKey{AccessKeyID: "AKIDTEST"}); err == nil {
		t.Fatal("a key without its secret was accepted")
	}
	conn, err := f.m.ConnectKey(ctx, byoc.AWS, byoc.AccessKey{AccessKeyID: "AKIDTEST", SecretAccessKey: "test-secret"})
	if err != nil || conn.Method != "access-key" {
		t.Fatalf("ConnectKey = %+v, %v", conn, err)
	}
	if _, err := f.m.ConnectKey(ctx, byoc.Azure, byoc.AccessKey{AccessKeyID: "x", SecretAccessKey: "y"}); !errors.Is(err, byoc.ErrUnsupported) {
		t.Fatalf("Azure's ConnectKey = %v", err)
	}
}

func TestSignInExpires(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	st, err := f.m.BeginSignIn(context.Background(), byoc.Azure, byoc.SignInOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.WaitSignIn(ctx, st.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitSignIn = %v", err)
	}
	f.m.CancelSignIn(st.ID)
	if _, err := f.m.SignInStatusOf(st.ID); !errors.Is(err, byoc.ErrNotFound) {
		t.Fatalf("a cancelled sign-in is still there: %v", err)
	}
}

func TestRegionsDefaultToTheClosest(t *testing.T) {
	f := newFixture(t)
	conn := f.connectAWS(t)
	rc, err := f.m.Regions(context.Background(), conn.ID)
	if err != nil || rc.Default != "eu-west-1" {
		t.Fatalf("from London the default is %q (%v)", rc.Default, err)
	}
}

func TestCreateScaleStopStartDelete(t *testing.T) {
	f := newFixture(t)
	conn := f.connectAWS(t)
	ctx := context.Background()
	var events []byoc.Event
	op, err := f.m.Create(ctx, byoc.CreateRequest{Instance: "dev", Connection: conn.ID, Region: "us-east-1", Preset: byoc.Medium, AutoStopMinutes: 30},
		true, func(e byoc.Event) { events = append(events, e) })
	if err != nil {
		t.Fatalf("Create = %v (%+v)", err, op)
	}
	if !op.Done || op.Machine == nil || op.Machine.State != byoc.Running || op.Machine.Size != "medium-16" || op.Machine.HourlyUSD != 0.8 {
		t.Fatalf("create op %+v", op)
	}
	spec := f.aws.Created[0]
	if spec.SourceCIDR != "203.0.113.9/32" || !strings.HasPrefix(spec.SSHPublicKey, "ssh-ed25519 ") || spec.SSHUser != "ubuntu" {
		t.Fatalf("spec %+v", spec)
	}
	if !strings.Contains(spec.UserData, "cloudburrow-idle.timer") || strings.Contains(spec.UserData, "BEGIN OPENSSH PRIVATE KEY-----\n"+"x") {
		t.Fatal("cloud-init lacks auto-stop")
	}
	key, err := f.keys.Get("machine/dev")
	if err != nil || !strings.Contains(string(key), "OPENSSH PRIVATE KEY") {
		t.Fatalf("the machine's key is not in the keychain: %v", err)
	}
	if strings.Contains(spec.UserData, string(key)) {
		t.Fatal("the client's private key went into the VM's user data")
	}
	if len(f.engine.connected) != 1 || f.engine.connected[0].Host != "198.51.100.7" || f.engine.connected[0].HostKey == "" {
		t.Fatalf("the remote engine got %+v", f.engine.connected)
	}
	steps := map[string]bool{}
	for _, e := range events {
		steps[e.Step] = true
	}
	for _, s := range []string{"permissions", "address", "key", "network", "vm", "remote"} {
		if !steps[s] {
			t.Errorf("no %s event in %v", s, events)
		}
	}
	if _, err := f.m.Create(ctx, byoc.CreateRequest{Instance: "dev", Connection: conn.ID, Region: "us-east-1"}, true, nil); err == nil {
		t.Fatal("a second create for the same instance was accepted")
	}

	if _, err := f.m.Scale(ctx, "dev", byoc.Large, "", true, nil); err != nil {
		t.Fatal(err)
	}
	if mc, _ := f.m.Machine("dev"); mc.Size != "large-32" || mc.Preset != byoc.Large || mc.HourlyUSD != 1.6 {
		t.Fatalf("after scale %+v", mc)
	}
	if _, err := f.m.Scale(ctx, "dev", byoc.GPU, "", true, nil); err == nil || !strings.Contains(err.Error(), "NVIDIA driver") {
		t.Fatalf("scaling to GPU = %v", err)
	}
	if _, err := f.m.Stop(ctx, "dev", true, nil); err != nil {
		t.Fatal(err)
	}
	if mc, _ := f.m.Machine("dev"); mc.State != byoc.Stopped {
		t.Fatalf("after stop %+v", mc)
	}
	if _, err := f.m.Start(ctx, "dev", true, nil); err != nil {
		t.Fatal(err)
	}
	if mc, _ := f.m.Refresh(ctx, "dev"); mc.State != byoc.Running {
		t.Fatalf("after start %+v", mc)
	}
	if err := f.m.Disconnect(conn.ID, false); err == nil || !strings.Contains(err.Error(), "still has machines") {
		t.Fatalf("Disconnect with a machine = %v", err)
	}
	if rs, _ := f.m.Resources(ctx, "dev"); len(rs) != 3 {
		t.Fatalf("resources %v", rs)
	}
	if _, err := f.m.Delete(ctx, "dev", true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Machine("dev"); !errors.Is(err, byoc.ErrNotFound) {
		t.Fatalf("the machine is still recorded: %v", err)
	}
	if _, err := f.keys.Get("machine/dev"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal("the machine's key outlived it")
	}
	if len(f.aws.Resources("dev")) != 0 {
		t.Fatal("the cloud still holds resources")
	}
}

func TestCreateRefusedForAMissingPermission(t *testing.T) {
	f := newFixture(t)
	conn := f.connectAWS(t)
	f.aws.Missing = []byoc.Permission{{Action: "ec2:RunInstances", Why: "to start the VM"}}
	op, err := f.m.Create(context.Background(), byoc.CreateRequest{Instance: "dev", Connection: conn.ID, Region: "us-east-1"}, true, nil)
	if err == nil || op.ErrorKind != "permission" || !strings.Contains(op.Error, "ec2:RunInstances (to start the VM)") {
		t.Fatalf("Create = %v, %+v", err, op)
	}
	if len(f.aws.Created) != 0 {
		t.Fatal("the provider was asked to create without the permission")
	}
	if _, err := f.m.Machine("dev"); !errors.Is(err, byoc.ErrNotFound) {
		t.Fatal("a refused create left a record")
	}
}

func TestCreateFailureIsRolledBackAndForgotten(t *testing.T) {
	f := newFixture(t)
	conn := f.connectAWS(t)
	f.aws.FailOn["create"] = &byoc.QuotaError{Message: "VcpuLimitExceeded: You have requested more vCPU capacity than your current vCPU limit"}
	op, err := f.m.Create(context.Background(), byoc.CreateRequest{Instance: "dev", Connection: conn.ID, Region: "us-east-1"}, true, nil)
	if err == nil || op.ErrorKind != "quota" {
		t.Fatalf("Create = %v, %+v", err, op)
	}
	rolled := false
	for _, e := range op.Events {
		rolled = rolled || e.Status == byoc.StepRolledBack
	}
	if !rolled || len(f.aws.Resources("dev")) != 0 {
		t.Fatalf("not rolled back: %+v", op.Events)
	}
	if _, err := f.m.Machine("dev"); !errors.Is(err, byoc.ErrNotFound) {
		t.Fatal("a failed create left a record")
	}
	if _, err := f.keys.Get("machine/dev"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal("a failed create left its key")
	}
}

func TestDeleteThatLeavesSomethingIsReportedAndKept(t *testing.T) {
	f := newFixture(t)
	conn := f.connectAWS(t)
	ctx := context.Background()
	if _, err := f.m.Create(ctx, byoc.CreateRequest{Instance: "dev", Connection: conn.ID, Region: "us-east-1"}, true, nil); err != nil {
		t.Fatal(err)
	}
	f.aws.Leave = true
	op, err := f.m.Delete(ctx, "dev", true, nil)
	if err == nil || op.ErrorKind != "leftover" || !strings.Contains(op.Error, "left 1 resources behind") {
		t.Fatalf("Delete = %v, %+v", err, op)
	}
	mc, err := f.m.Machine("dev")
	if err != nil || mc.State != byoc.Failed {
		t.Fatalf("the record after a failed delete: %+v, %v", mc, err)
	}
	f.aws.Leave = false
	if _, err := f.m.Delete(ctx, "dev", true, nil); err != nil {
		t.Fatalf("a second Delete = %v", err)
	}
}

func TestAsyncOperationsAndBusy(t *testing.T) {
	f := newFixture(t)
	conn := f.connectAWS(t)
	ctx := context.Background()
	op, err := f.m.Create(ctx, byoc.CreateRequest{Instance: "dev", Connection: conn.ID, Region: "us-east-1"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := f.m.Operation(op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Done {
			if got.Error != "" {
				t.Fatal(got.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the create did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ops := f.m.Operations(); len(ops) != 1 || ops[0].ID != op.ID {
		t.Fatalf("operations %+v", ops)
	}
}

func TestInvalidRequests(t *testing.T) {
	f := newFixture(t)
	conn := f.connectAWS(t)
	ctx := context.Background()
	for _, req := range []byoc.CreateRequest{
		{Instance: "Dev", Connection: conn.ID},
		{Instance: "a", Connection: conn.ID},
		{Instance: "dev", Connection: conn.ID, AutoStopMinutes: -1},
		{Instance: "dev", Connection: "aws-000000000000"},
	} {
		if _, err := f.m.Create(ctx, req, true, nil); err == nil {
			t.Errorf("Create(%+v) was accepted", req)
		}
	}
	if _, err := f.m.Scale(ctx, "nope", byoc.Small, "", true, nil); !errors.Is(err, byoc.ErrNotFound) {
		t.Errorf("Scale of no machine = %v", err)
	}
}

func TestResolveConnection(t *testing.T) {
	f := newFixture(t)
	if _, err := f.m.ResolveConnection(byoc.AWS, ""); err == nil || !strings.Contains(err.Error(), "cloud connect aws") {
		t.Fatalf("no connection: %v", err)
	}
	conn := f.connectAWS(t)
	got, err := f.m.ResolveConnection(byoc.AWS, "")
	if err != nil || got.ID != conn.ID {
		t.Fatalf("ResolveConnection = %+v, %v", got, err)
	}
	if _, err := f.m.ConnectKey(context.Background(), byoc.AWS, byoc.AccessKey{AccessKeyID: "A", SecretAccessKey: "B"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.ResolveConnection(byoc.AWS, ""); err == nil || !strings.Contains(err.Error(), "--connection") {
		t.Fatalf("two connections: %v", err)
	}
}

func TestStateFileIsJSONWithoutSecrets(t *testing.T) {
	f := newFixture(t)
	conn := f.connectAWS(t)
	if _, err := f.m.Create(context.Background(), byoc.CreateRequest{Instance: "dev", Connection: conn.ID, Region: "us-east-1"}, true, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(f.dir, "byoc.json"))
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "PRIVATE KEY") {
		t.Fatal("a private key is in the state file")
	}
}
