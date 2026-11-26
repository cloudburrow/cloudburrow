package byoc_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
)

// runnerEngine is a recordingEngine that also runs commands, as the
// remotecompute engine does over SSH, keeping the VM's auto-stop file.
type runnerEngine struct {
	recordingEngine
	mu       sync.Mutex
	commands []string
	file     map[string]string // instance -> the auto-stop file
	fail     error
}

func (e *runnerEngine) Run(_ context.Context, t byoc.RemoteTarget, command string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.commands = append(e.commands, command)
	if e.fail != nil {
		return "", e.fail
	}
	// The command writes one number then cats the file.
	i := strings.Index(command, "printf '%s\\n' ")
	n := strings.Fields(command[i+len("printf '%s\\n' "):])[0]
	if e.file == nil {
		e.file = map[string]string{}
	}
	e.file[t.Instance] = n
	return n + "\n", nil
}

type fakeTunnel struct {
	port   int
	closed bool
}

func (t *fakeTunnel) Port() int        { return t.port }
func (t *fakeTunnel) LastError() error { return nil }
func (t *fakeTunnel) Close() error     { t.closed = true; return nil }

func TestAWSDefaultsToSessionManagerWithNothingInbound(t *testing.T) {
	f := newFixture(t)
	var tunnels []*fakeTunnel
	f.aws.Tunnel = func(m byoc.Machine, port int) (byoc.Tunnel, error) {
		if port != 22 {
			t.Errorf("tunnel to port %d", port)
		}
		tun := &fakeTunnel{port: 40000 + len(tunnels)}
		tunnels = append(tunnels, tun)
		return tun, nil
	}
	conn := f.connectAWS(t)
	ctx := context.Background()
	op, err := f.m.Create(ctx, byoc.CreateRequest{Instance: "ssmbox", Connection: conn.ID, Region: "us-east-1"}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := f.aws.Created[0]
	if spec.ConnectMode != byoc.ConnectSSM || spec.SourceCIDR != "" {
		t.Fatalf("spec mode %q, source %q: SSM opens nothing inbound", spec.ConnectMode, spec.SourceCIDR)
	}
	if !strings.Contains(spec.UserData, "amazon-ssm-agent") {
		t.Error("the SSM-mode cloud-init does not start the SSM agent")
	}
	if op.Machine.ConnectMode != byoc.ConnectSSM {
		t.Errorf("machine mode %q", op.Machine.ConnectMode)
	}
	if len(f.engine.connected) != 1 || f.engine.connected[0].Host != "127.0.0.1" || f.engine.connected[0].Port != 40000 || f.engine.connected[0].Via != byoc.ConnectSSM {
		t.Fatalf("the engine was handed %+v; want the tunnel on 127.0.0.1:40000", f.engine.connected)
	}
	// The tunnel is kept for the process, and closed when the VM stops.
	if _, err := f.m.RemoteTarget("ssmbox"); err != nil || len(tunnels) != 1 {
		t.Fatalf("a second target opened another tunnel (%d) %v", len(tunnels), err)
	}
	if _, err := f.m.Stop(ctx, "ssmbox", true, nil); err != nil {
		t.Fatal(err)
	}
	if !tunnels[0].closed {
		t.Error("Stop left the tunnel open")
	}
	if _, err := f.m.AllowMyAddress(ctx, "ssmbox", true, nil); err == nil || !strings.Contains(err.Error(), "nothing inbound") {
		t.Errorf("allow-my-address on an SSM VM: %v", err)
	}
	// SSH stays a choice.
	op, err = f.m.Create(ctx, byoc.CreateRequest{Instance: "sshbox", Connection: conn.ID, Region: "us-east-1", ConnectMode: byoc.ConnectSSH}, true, nil)
	if err != nil || op.Machine.ConnectMode != byoc.ConnectSSH || f.aws.Created[1].SourceCIDR != "203.0.113.9/32" {
		t.Fatalf("SSH fallback: %v %+v", err, f.aws.Created[1])
	}
}

func TestAzureRefusesSessionManager(t *testing.T) {
	f := newFixture(t)
	f.azure.Approve()
	ctx := context.Background()
	st, err := f.m.BeginSignIn(ctx, byoc.Azure, byoc.SignInOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.WaitSignIn(ctx, st.ID); err != nil {
		t.Fatal(err)
	}
	conn, err := f.m.ChooseAccount(ctx, st.ID, "111122223333")
	if err != nil {
		t.Fatal(err)
	}
	op, err := f.m.Create(ctx, byoc.CreateRequest{Instance: "azbox", Connection: conn.ID, Region: "eastus", ConnectMode: byoc.ConnectSSM}, true, nil)
	if err == nil || !strings.Contains(err.Error(), "Azure has no Session Manager") {
		t.Fatalf("Azure SSM create = %v %+v", err, op)
	}
	if _, err := f.m.Create(ctx, byoc.CreateRequest{Instance: "azbox", Connection: conn.ID, Region: "eastus"}, true, nil); err != nil {
		t.Fatal(err)
	}
	if f.azure.Created[len(f.azure.Created)-1].ConnectMode != byoc.ConnectSSH {
		t.Error("Azure did not default to SSH")
	}
}

func TestFirewallFollowsTheUsersAddress(t *testing.T) {
	f := newFixture(t)
	addr := "203.0.113.9"
	var lookups int
	m, err := byoc.NewManager(byoc.Options{Dir: f.dir, Keychain: f.keys, Drivers: []byoc.Driver{f.aws, f.azure}, Engine: f.engine,
		IPLookup:    func(context.Context) (string, error) { lookups++; return addr, nil },
		OpenBrowser: func(u string) error { f.opened = append(f.opened, u); return nil },
		Location:    func() (float64, float64, bool) { return 51.5, -0.1, true }})
	if err != nil {
		t.Fatal(err)
	}
	f.m = m
	conn := f.connectAWS(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, byoc.CreateRequest{Instance: "dev", Connection: conn.ID, Region: "us-east-1"}, true, nil); err != nil {
		t.Fatal(err)
	}
	mc, _ := m.Machine("dev")
	if mc.ConnectMode != byoc.ConnectSSH || mc.SourceCIDR != "203.0.113.9/32" || f.aws.Source("dev") != "203.0.113.9/32" {
		t.Fatalf("after create: %+v, cloud %q", mc, f.aws.Source("dev"))
	}
	// Unchanged: nothing moves.
	if _, changed, err := m.FollowAddress(ctx, "dev", nil); err != nil || changed {
		t.Fatalf("unchanged address moved the rule: %v %v", changed, err)
	}
	// The user moves to another network: the next connect moves the rule
	// first, and each reconnect checks again.
	addr = "198.51.100.44"
	tgt, err := m.RemoteTarget("dev")
	if err != nil {
		t.Fatal(err)
	}
	if f.aws.Source("dev") != "198.51.100.44/32" {
		t.Fatalf("the firewall still admits %q", f.aws.Source("dev"))
	}
	addr = "2001:db8::7"
	if err := tgt.BeforeConnect(ctx); err != nil || f.aws.Source("dev") != "2001:db8::7/128" {
		t.Fatalf("BeforeConnect: %v, source %q", err, f.aws.Source("dev"))
	}
	mc, _ = m.Machine("dev")
	if mc.SourceCIDR != "2001:db8::7/128" || mc.SourceUpdatedAt.IsZero() {
		t.Errorf("record %+v", mc)
	}
	if ch := m.AddressChanges("dev"); len(ch) != 2 || ch[0].From != "203.0.113.9/32" || ch[1].To != "2001:db8::7/128" {
		t.Errorf("changes %+v", ch)
	}
	// The manual trigger, and a refused update said in words.
	addr = "192.0.2.1"
	op, err := m.AllowMyAddress(ctx, "dev", true, nil)
	if err != nil || f.aws.Source("dev") != "192.0.2.1/32" || len(op.Events) == 0 {
		t.Fatalf("AllowMyAddress = %v %+v", err, op)
	}
	addr = "192.0.2.2"
	f.aws.FailOn["address"] = &byoc.PermissionError{Message: "ec2:RevokeSecurityGroupIngress"}
	if _, err := m.AllowMyAddress(ctx, "dev", true, nil); err == nil || !strings.Contains(err.Error(), "RevokeSecurityGroupIngress") {
		t.Errorf("a refused update: %v", err)
	}
	if mc, _ := m.Machine("dev"); mc.SourceCIDR != "192.0.2.1/32" {
		t.Errorf("a refused update changed the record to %q", mc.SourceCIDR)
	}
	// The watcher moves running machines' rules.
	delete(f.aws.FailOn, "address")
	wctx, cancel := context.WithCancel(ctx)
	cancel()
	m.WatchAddresses(wctx, 0, nil)
	if f.aws.Source("dev") != "192.0.2.2/32" {
		t.Errorf("the watcher left the rule at %q", f.aws.Source("dev"))
	}
}

func TestAutoStopIsEditableAfterCreate(t *testing.T) {
	f := newFixture(t)
	eng := &runnerEngine{}
	m, err := byoc.NewManager(byoc.Options{Dir: f.dir, Keychain: f.keys, Drivers: []byoc.Driver{f.aws}, Engine: eng,
		IPLookup:    func(context.Context) (string, error) { return "203.0.113.9", nil },
		OpenBrowser: func(u string) error { f.opened = append(f.opened, u); return nil },
		Location:    func() (float64, float64, bool) { return 51.5, -0.1, true }})
	if err != nil {
		t.Fatal(err)
	}
	f.m = m
	conn := f.connectAWS(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, byoc.CreateRequest{Instance: "dev", Connection: conn.ID, Region: "us-east-1", AutoStopMinutes: 60}, true, nil); err != nil {
		t.Fatal(err)
	}
	// The create's hand-over writes the setting it recorded.
	if eng.file["dev"] != "60" {
		t.Fatalf("after create the VM's setting is %q", eng.file["dev"])
	}
	op, err := m.SetAutoStop(ctx, "dev", 30, true, nil)
	if err != nil || op.Machine.AutoStopMinutes != 30 || eng.file["dev"] != "30" {
		t.Fatalf("SetAutoStop(30) = %v, machine %+v, VM %q", err, op.Machine, eng.file["dev"])
	}
	if mc, _ := m.Machine("dev"); mc.AutoStopMinutes != 30 {
		t.Error("not recorded")
	}
	// A refused write keeps the old value, said in words.
	eng.fail = errors.New("ssh: connection refused")
	if _, err := m.SetAutoStop(ctx, "dev", 0, true, nil); err == nil || !strings.Contains(err.Error(), "left at after 30 minutes idle") {
		t.Fatalf("a refused write: %v", err)
	}
	if mc, _ := m.Machine("dev"); mc.AutoStopMinutes != 30 {
		t.Errorf("a refused write changed the record to %d", mc.AutoStopMinutes)
	}
	eng.fail = nil
	// On a stopped VM it is recorded and written at the next start.
	if _, err := m.Stop(ctx, "dev", true, nil); err != nil {
		t.Fatal(err)
	}
	n := len(eng.commands)
	op, err = m.SetAutoStop(ctx, "dev", 120, true, nil)
	if err != nil || len(eng.commands) != n || !strings.Contains(op.Events[len(op.Events)-1].Message, "next starts") {
		t.Fatalf("stopped: %v, commands %d->%d, %+v", err, n, len(eng.commands), op.Events)
	}
	if _, err := m.Start(ctx, "dev", true, nil); err != nil {
		t.Fatal(err)
	}
	if eng.file["dev"] != "120" {
		t.Errorf("after start the VM's setting is %q", eng.file["dev"])
	}
	if _, err := m.SetAutoStop(ctx, "dev", byoc.MaxAutoStopMinutes+1, true, nil); err == nil {
		t.Error("a week and a minute was accepted")
	}
	for _, c := range []struct {
		n    int
		want string
	}{{0, "never"}, {45, "after 45 minutes idle"}, {60, "after 1 hour idle"}, {180, "after 3 hours idle"}} {
		if got := byoc.AutoStopWords(c.n); got != c.want {
			t.Errorf("AutoStopWords(%d) = %q", c.n, got)
		}
	}
}
