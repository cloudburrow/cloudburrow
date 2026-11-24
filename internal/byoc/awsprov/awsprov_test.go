package awsprov_test

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov"
	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov/awsfake"
	"github.com/cloudburrow/cloudburrow/internal/sched"
)

// Every test talks to awsfake on loopback with credentials it makes up; none
// reads ~/.aws or reaches AWS.

const startURL = "https://d-0000000000.awsapps.com/start"

func fake(t *testing.T) (*awsfake.Server, *awsprov.Driver) {
	t.Helper()
	f := awsfake.New()
	t.Cleanup(f.Close)
	return f, awsprov.NewDriver(f.Options())
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// keyProvider connects with a made-up access key.
func keyProvider(t *testing.T, f *awsfake.Server, d *awsprov.Driver) byoc.Provider {
	t.Helper()
	conn, sec, err := d.ConnectKey(ctx(t), byoc.AccessKey{AccessKeyID: "AKIAFAKETEST00000001", SecretAccessKey: "fake-secret"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := d.Open(ctx(t), conn, sec, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type events struct {
	mu  sync.Mutex
	all []byoc.Event
}

func (e *events) progress() byoc.Progress {
	return func(ev byoc.Event) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.all = append(e.all, ev)
	}
}

func (e *events) has(step string, status byoc.EventStatus) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.all {
		if ev.Step == step && ev.Status == status {
			return true
		}
	}
	return false
}

func spec(instance string) byoc.MachineSpec {
	return byoc.MachineSpec{Instance: instance, Region: "us-east-1", Size: "m7i.xlarge", Preset: byoc.Small,
		AutoStopMinutes: 30, SSHUser: "cloudburrow", SourceCIDR: "203.0.113.7/32", UserData: "#cloud-config\nruncmd: []\n"}
}

func TestDeviceFlowSignIn(t *testing.T) {
	f := awsfake.New()
	t.Cleanup(f.Close)
	// The polls are paced by a clock the test moves, so no poll waits.
	clk := sched.NewFakeClock(time.Now())
	opts := f.Options()
	opts.Clock = clk
	d := awsprov.NewDriver(opts)
	f.SetAccounts([]awsfake.Account{
		{ID: "111122223333", Name: "Sandbox", Roles: []string{"AdministratorAccess", "PowerUserAccess"}},
		{ID: "444455556666", Name: "Production", Roles: []string{"ReadOnly"}},
	})
	var opened string
	s, err := d.BeginSignIn(ctx(t), byoc.SignInOptions{StartURL: startURL, Region: "us-east-1",
		OpenBrowser: func(u string) error { opened = u; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	pr := s.Prompt()
	if pr.UserCode == "" || !strings.Contains(pr.URL, "user_code="+pr.UserCode) || opened != pr.URL {
		t.Fatalf("prompt %+v, opened %q", pr, opened)
	}
	// The user approves only after a few polls answer pending.
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Two polls answer pending; the user approves before the third.
		for polls := 1; polls <= 2; polls++ {
			for clk.Waiters() == 0 {
				runtime.Gosched()
			}
			if polls == 2 {
				f.Approve()
			}
			clk.Advance(time.Second)
		}
	}()
	choices, err := s.Wait(ctx(t))
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if n := f.Count("sso-oauth:CreateToken"); n != 3 {
		t.Errorf("%d CreateToken polls, want 2 pending and 1 approved", n)
	}
	if len(choices) != 3 {
		t.Fatalf("choices %+v", choices)
	}
	want := "111122223333/PowerUserAccess"
	var label string
	for _, c := range choices {
		if c.ID == want {
			label = c.Label
		}
	}
	if label != "Sandbox (111122223333) - PowerUserAccess" {
		t.Errorf("label %q", label)
	}
	conn, sec, err := s.Choose(ctx(t), want)
	if err != nil {
		t.Fatal(err)
	}
	if conn.ID != "aws-111122223333" || conn.Method != "sso" || conn.Detail["role"] != "PowerUserAccess" ||
		conn.Detail["start_url"] != startURL || !strings.Contains(conn.Principal, "assumed-role/AWSReservedSSO_PowerUserAccess") {
		t.Errorf("connection %+v", conn)
	}
	var stored map[string]any
	if err := json.Unmarshal(sec, &stored); err != nil || stored["sso"] == nil || stored["access_key_id"] == "" {
		t.Errorf("secret is not the sign-in's state: %v", err)
	}
	if _, _, err := s.Choose(ctx(t), "999/Nope"); err == nil {
		t.Error("an account the sign-in did not list was connected")
	}

	p, err := d.Open(ctx(t), conn, sec, nil)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := p.Connect(ctx(t))
	if err != nil || acct.ID != "111122223333" {
		t.Errorf("Connect = %+v, %v", acct, err)
	}
}

func TestDeviceFlowExpiredCode(t *testing.T) {
	f, d := fake(t)
	s, err := d.BeginSignIn(ctx(t), byoc.SignInOptions{StartURL: startURL, Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	f.ExpireDevice()
	if _, err := s.Wait(ctx(t)); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("Wait = %v, want the code's expiry", err)
	}
}

func TestBeginSignInNeedsPortalAndRegion(t *testing.T) {
	_, d := fake(t)
	for _, o := range []byoc.SignInOptions{{Region: "us-east-1"}, {StartURL: startURL}, {StartURL: "http://insecure", Region: "us-east-1"}} {
		if _, err := d.BeginSignIn(ctx(t), o); err == nil {
			t.Errorf("BeginSignIn(%+v) started", o)
		}
	}
}

func TestNoAccountAssigned(t *testing.T) {
	f, d := fake(t)
	f.SetAccounts(nil)
	s, err := d.BeginSignIn(ctx(t), byoc.SignInOptions{StartURL: startURL, Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	f.Approve()
	if _, err := s.Wait(ctx(t)); err == nil || !strings.Contains(err.Error(), "no AWS account") {
		t.Errorf("Wait = %v", err)
	}
}

// signIn connects through the device flow.
func signIn(t *testing.T, f *awsfake.Server, d *awsprov.Driver) (byoc.Connection, []byte) {
	t.Helper()
	s, err := d.BeginSignIn(ctx(t), byoc.SignInOptions{StartURL: startURL, Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	f.Approve()
	if _, err := s.Wait(ctx(t)); err != nil {
		t.Fatal(err)
	}
	conn, sec, err := s.Choose(ctx(t), awsfake.DefaultAccount+"/"+awsfake.DefaultRole)
	if err != nil {
		t.Fatal(err)
	}
	return conn, sec
}

func TestCredentialRefresh(t *testing.T) {
	f, d := fake(t)
	// Role credentials that are always inside the five-minute renewal
	// window, and an access token that has already expired: every call
	// renews the token with the refresh token and the role credentials
	// with it.
	f.SetTTLs(0, time.Minute, 0)
	conn, sec := signIn(t, f, d)
	var saved [][]byte
	p, err := d.Open(ctx(t), conn, sec, func(b []byte) error { saved = append(saved, b); return nil })
	if err != nil {
		t.Fatal(err)
	}
	before := f.Count("sso:GetRoleCredentials")
	if _, err := p.Connect(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if f.Count("sso:GetRoleCredentials") <= before || len(saved) == 0 {
		t.Errorf("no renewal: %d GetRoleCredentials, %d saves", f.Count("sso:GetRoleCredentials")-before, len(saved))
	}
	// A revoked access token is renewed once with the refresh token.
	f.RevokeTokens()
	n := f.Count("sso-oauth:CreateToken")
	if _, err := p.Connect(ctx(t)); err != nil {
		t.Fatalf("after the token was revoked: %v", err)
	}
	if f.Count("sso-oauth:CreateToken") != n+1 {
		t.Errorf("the refresh token was not used")
	}
	// With the refresh token gone too, the user must sign in again.
	f.RevokeAll()
	_, err = p.Connect(ctx(t))
	var auth *byoc.AuthError
	if !errors.As(err, &auth) {
		t.Errorf("Connect = %v, want an AuthError", err)
	}
}

func TestConnectKey(t *testing.T) {
	f, d := fake(t)
	conn, sec, err := d.ConnectKey(ctx(t), byoc.AccessKey{AccessKeyID: " AKIAFAKETEST00000001 ", SecretAccessKey: "fake-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if conn.ID != "aws-"+awsfake.DefaultAccount || conn.Method != "access-key" || conn.Principal != awsfake.UserARN {
		t.Errorf("connection %+v", conn)
	}
	if !strings.Contains(string(sec), "AKIAFAKETEST00000001") {
		t.Error("the secret does not hold the key")
	}
	if _, _, err := d.ConnectKey(ctx(t), byoc.AccessKey{AccessKeyID: "AKIA"}); err == nil {
		t.Error("a key without its secret connected")
	}
	f.RejectKey("AKIAREJECTED00000001")
	_, _, err = d.ConnectKey(ctx(t), byoc.AccessKey{AccessKeyID: "AKIAREJECTED00000001", SecretAccessKey: "x"})
	var auth *byoc.AuthError
	if !errors.As(err, &auth) {
		t.Errorf("a rejected key: %v, want an AuthError", err)
	}
}

func TestCheckPermissions(t *testing.T) {
	f, d := fake(t)
	p := keyProvider(t, f, d)
	rep, err := p.CheckPermissions(ctx(t), "us-east-1")
	if err != nil || !rep.Checked || !rep.OK() {
		t.Fatalf("all granted: %+v, %v", rep, err)
	}
	f.Deny("ec2:RunInstances")
	f.Deny("ec2:DeleteVpc")
	rep, err = p.CheckPermissions(ctx(t), "us-east-1")
	if err != nil || rep.OK() || len(rep.Missing) != 2 {
		t.Fatalf("two denied: %+v, %v", rep, err)
	}
	if s := rep.Summary(); !strings.Contains(s, "ec2:RunInstances (create the VM and its disk)") || !strings.Contains(s, "ec2:DeleteVpc") {
		t.Errorf("summary %q", s)
	}
	// The check itself refused: say so rather than claim anything.
	f.Deny("iam:SimulatePrincipalPolicy")
	rep, err = p.CheckPermissions(ctx(t), "us-east-1")
	if err != nil || rep.Checked || rep.Note == "" {
		t.Errorf("unchecked: %+v, %v", rep, err)
	}
}

func TestCheckPermissionsOfAnSSORole(t *testing.T) {
	f, d := fake(t)
	conn, sec := signIn(t, f, d)
	p, err := d.Open(ctx(t), conn, sec, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Deny("ec2:CreateSecurityGroup")
	rep, err := p.CheckPermissions(ctx(t), "us-east-1")
	if err != nil || len(rep.Missing) != 1 || rep.Missing[0].Action != "ec2:CreateSecurityGroup" {
		t.Errorf("%+v, %v", rep, err)
	}
	if f.Count("iam:GetRole") != 1 {
		t.Error("the role's ARN was not read")
	}
}

func TestRegionsAndSizes(t *testing.T) {
	f, d := fake(t)
	p := keyProvider(t, f, d)
	rs, err := p.Regions(ctx(t))
	if err != nil || len(rs) != 5 {
		t.Fatalf("regions %+v, %v", rs, err)
	}
	for _, r := range rs {
		if r.Name == "eu-west-1" && (r.DisplayName != "Europe (Ireland)" || r.Latitude == 0) {
			t.Errorf("eu-west-1 = %+v", r)
		}
	}
	sz, err := p.Sizes(ctx(t), "us-east-1")
	if err != nil || len(sz) != 4 {
		t.Fatalf("sizes %+v, %v", sz, err)
	}
	if sz[0].Name != "m7i.xlarge" || sz[0].HourlyUSD != 0.2016 || sz[0].VCPUs != 4 || sz[0].MemoryGiB != 16 {
		t.Errorf("small %+v", sz[0])
	}
	if g := sz[3]; g.Preset != byoc.GPU || g.Name != "g6.xlarge" || g.GPU != "NVIDIA L4" || g.HourlyUSD != 0.8048 {
		t.Errorf("gpu %+v", g)
	}
	if d := sz[0].DiskHourlyUSD; d < 0.0109 || d > 0.011 {
		t.Errorf("disk %v per hour, want 100 GiB at $0.08/GB-month", d)
	}
	// A region without g6: the size says it has no price rather than 0.
	sz, err = p.Sizes(ctx(t), "eu-west-1")
	if err != nil || sz[3].PriceError == "" || sz[3].HourlyUSD != 0 || sz[0].HourlyUSD != 0.2226 {
		t.Errorf("eu-west-1 sizes %+v, %v", sz, err)
	}
	// The Price List API refusing is a price error, not a failure.
	f.Deny("pricing:GetProducts")
	sz, err = p.Sizes(ctx(t), "us-east-1")
	if err != nil || sz[0].PriceError == "" {
		t.Errorf("pricing denied: %+v, %v", sz, err)
	}
}

func TestCreateStatusResizeStopStartDelete(t *testing.T) {
	f, d := fake(t)
	p := keyProvider(t, f, d)
	ev := &events{}
	m, err := p.Create(ctx(t), spec("alpha"), ev.progress())
	if err != nil {
		t.Fatal(err)
	}
	if m.State != byoc.Running || m.PublicIP == "" || m.IDs["instance"] == "" || m.IDs["volume"] == "" || m.SSHPort != 22 ||
		m.Connection != "aws-"+awsfake.DefaultAccount || m.Preset != byoc.Small {
		t.Errorf("machine %+v", m)
	}
	for _, step := range []string{"network", "firewall", "image", "vm", "boot"} {
		if !ev.has(step, byoc.StepDone) {
			t.Errorf("no %s done event", step)
		}
	}
	live := f.Live()
	kinds := map[string]int{}
	for _, r := range live {
		kinds[r.Kind]++
		if r.Kind == "route-table" && r.Tags[byoc.TagKey] == "" {
			continue // the VPC's main route table, deleted with the VPC
		}
		if r.Tags[byoc.TagKey] != "alpha" {
			t.Errorf("%s %s is not tagged: %v", r.Kind, r.ID, r.Tags)
		}
		switch r.Kind {
		case "security-group":
			if len(r.Ingress) != 1 || r.Ingress[0] != "tcp 22-22 203.0.113.7/32" {
				t.Errorf("the security group lets in %v; want SSH from the user's address only", r.Ingress)
			}
		case "instance":
			if r.ShutdownBehavior != "stop" || !strings.HasPrefix(r.UserData, "#cloud-config") || r.InstanceType != "m7i.xlarge" {
				t.Errorf("instance %+v", r)
			}
		case "volume":
			if r.VolumeSize != 100 || !r.Encrypted {
				t.Errorf("volume %+v", r)
			}
		}
	}
	for _, k := range []string{"vpc", "subnet", "internet-gateway", "security-group", "instance", "volume"} {
		if kinds[k] != 1 {
			t.Errorf("%d %s, want 1", kinds[k], k)
		}
	}
	res, err := p.ListResources(ctx(t), "alpha", "us-east-1")
	if err != nil || len(res) != 7 {
		t.Errorf("ListResources = %d %+v, %v", len(res), res, err)
	}

	m, err = p.Stop(ctx(t), m)
	if err != nil || m.State != byoc.Stopped {
		t.Fatalf("Stop = %+v, %v", m.State, err)
	}
	m, err = p.Start(ctx(t), m)
	if err != nil || m.State != byoc.Running {
		t.Fatalf("Start = %+v, %v", m.State, err)
	}
	ev = &events{}
	m, err = p.Resize(ctx(t), m, "m7i.4xlarge", ev.progress())
	if err != nil || m.Size != "m7i.4xlarge" || m.Preset != byoc.Medium || m.State != byoc.Running {
		t.Fatalf("Resize = %+v, %v", m, err)
	}
	if !ev.has("stop", byoc.StepDone) || !ev.has("start", byoc.StepDone) {
		t.Errorf("a running VM was not stopped and started around the resize: %+v", ev.all)
	}
	if st, err := p.Status(ctx(t), m); err != nil || st.Size != "m7i.4xlarge" || st.State != byoc.Running {
		t.Errorf("Status = %+v, %v", st, err)
	}

	ev = &events{}
	if err := p.Delete(ctx(t), "alpha", "us-east-1", ev.progress()); err != nil {
		t.Fatal(err)
	}
	if left := f.Live(); len(left) != 0 {
		t.Errorf("left after delete: %+v", left)
	}
	if !ev.has("verify", byoc.StepDone) {
		t.Error("delete did not verify")
	}
	if st, err := p.Status(ctx(t), m); err != nil || st.State != byoc.Deleted {
		t.Errorf("Status after delete = %v, %v", st.State, err)
	}
}

func TestCreateQuotaExceededRollsBack(t *testing.T) {
	f, d := fake(t)
	p := keyProvider(t, f, d)
	f.FailNext("ec2:RunInstances", "VcpuLimitExceeded",
		"You have requested more vCPU capacity than your current vCPU limit of 0 allows for the instance bucket that the specified instance type belongs to.")
	ev := &events{}
	_, err := p.Create(ctx(t), spec("quota"), ev.progress())
	var q *byoc.QuotaError
	if !errors.As(err, &q) || !strings.Contains(err.Error(), "VcpuLimitExceeded") {
		t.Fatalf("Create = %v, want a QuotaError", err)
	}
	if left := f.Live(); len(left) != 0 {
		t.Errorf("the failed create left %+v", left)
	}
	if !ev.has("rollback", byoc.StepRolledBack) || !ev.has("vm", byoc.StepFailed) {
		t.Errorf("events %+v", ev.all)
	}
}

func TestCreatePermissionMissingRollsBack(t *testing.T) {
	f, d := fake(t)
	p := keyProvider(t, f, d)
	f.Deny("ec2:CreateSecurityGroup")
	_, err := p.Create(ctx(t), spec("denied"), nil)
	var pe *byoc.PermissionError
	if !errors.As(err, &pe) || len(pe.Missing) != 1 || pe.Missing[0].Action != "ec2:CreateSecurityGroup" ||
		!strings.Contains(err.Error(), "create the VM's firewall") {
		t.Fatalf("Create = %v, want the missing permission named", err)
	}
	if left := f.Live(); len(left) != 0 {
		t.Errorf("the failed create left %+v", left)
	}
}

func TestCreateWithoutSourceOpensNothing(t *testing.T) {
	f, d := fake(t)
	p := keyProvider(t, f, d)
	sp := spec("closed")
	sp.SourceCIDR = ""
	if _, err := p.Create(ctx(t), sp, nil); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.Live() {
		if r.Kind == "security-group" && len(r.Ingress) != 0 {
			t.Errorf("inbound rules %v, want none", r.Ingress)
		}
	}
	if f.Count("ec2:AuthorizeSecurityGroupIngress") != 0 {
		t.Error("ingress was authorized")
	}
}

func TestDeleteVerifyFindsLeftovers(t *testing.T) {
	f, d := fake(t)
	p := keyProvider(t, f, d)
	if _, err := p.Create(ctx(t), spec("sticky"), nil); err != nil {
		t.Fatal(err)
	}
	f.KeepOnDelete("security-group")
	err := p.Delete(ctx(t), "sticky", "us-east-1", nil)
	var lo *byoc.LeftoverError
	if !errors.As(err, &lo) {
		t.Fatalf("Delete = %v, want a LeftoverError", err)
	}
	types := map[string]bool{}
	for _, r := range lo.Resources {
		types[r.Type] = true
	}
	if !types["ec2:security-group"] || !types["ec2:vpc"] {
		t.Errorf("leftovers %+v; the group and the VPC it holds should be named", lo.Resources)
	}
}

func TestDeleteOnlyTouchesTheInstancesTag(t *testing.T) {
	f, d := fake(t)
	p := keyProvider(t, f, d)
	if _, err := p.Create(ctx(t), spec("one"), nil); err != nil {
		t.Fatal(err)
	}
	sp := spec("two")
	if _, err := p.Create(ctx(t), sp, nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(ctx(t), "one", "us-east-1", nil); err != nil {
		t.Fatal(err)
	}
	res, err := p.ListResources(ctx(t), "two", "us-east-1")
	if err != nil || len(res) != 7 {
		t.Errorf("instance two has %d resources after one was deleted: %v", len(res), err)
	}
	if res, _ := p.ListResources(ctx(t), "one", "us-east-1"); len(res) != 0 {
		t.Errorf("instance one still has %+v", res)
	}
}

func TestExpiredCredentialsAreAnAuthError(t *testing.T) {
	f, d := fake(t)
	f.RejectKey("AKIAFAKETEST00000001")
	conn := byoc.Connection{ID: "aws-1", Cloud: byoc.AWS, Method: "access-key"}
	p, err := d.Open(ctx(t), conn, []byte(`{"method":"access-key","access_key_id":"AKIAFAKETEST00000001","secret_access_key":"x"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Create(ctx(t), spec("auth"), nil)
	var auth *byoc.AuthError
	if !errors.As(err, &auth) {
		t.Errorf("Create = %v, want an AuthError", err)
	}
}
