package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov"
	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov/awsfake"
	"github.com/cloudburrow/cloudburrow/internal/byoc/azureprov"
	"github.com/cloudburrow/cloudburrow/internal/byoc/azureprov/azurefake"
	"github.com/cloudburrow/cloudburrow/internal/byoc/keychain"
	"github.com/cloudburrow/cloudburrow/internal/config"
)

// cloudFixture points `cloudburrow cloud` at the local AWS and Azure fakes,
// with an in-memory keychain: nothing leaves the machine and the real
// keychain is never read.
type cloudFixture struct {
	aws    *awsfake.Server
	az     *azurefake.Server
	keys   *keychain.Memory
	mu     sync.Mutex
	opened []string
	stdin  *bytes.Buffer
}

func newCloudFixture(t *testing.T) *cloudFixture {
	t.Helper()
	f := &cloudFixture{aws: awsfake.New(), az: azurefake.New(), keys: keychain.NewMemory(), stdin: &bytes.Buffer{}}
	t.Cleanup(f.aws.Close)
	t.Cleanup(f.az.Close)
	var m *byoc.Manager
	prevNew, prevIn := newCloudManager, cloudStdin
	newCloudManager = func(cfg config.Config) (*byoc.Manager, error) {
		if m != nil {
			return m, nil
		}
		var err error
		m, err = byoc.NewManager(byoc.Options{
			Dir: t.TempDir(), Keychain: f.keys,
			Drivers:  []byoc.Driver{awsprov.NewDriver(f.aws.Options()), azureprov.New(f.az.Options())},
			IPLookup: func(context.Context) (string, error) { return "203.0.113.50", nil },
			OpenBrowser: func(u string) error {
				f.mu.Lock()
				f.opened = append(f.opened, u)
				f.mu.Unlock()
				if strings.HasPrefix(u, f.az.URL()) {
					return f.az.OpenBrowser(u)
				}
				// The user confirms the code on AWS's page.
				go func() { time.Sleep(20 * time.Millisecond); f.aws.Approve() }()
				return nil
			},
			Location: func() (float64, float64, bool) { return 40.7, -74.0, true },
		})
		return m, err
	}
	cloudStdin = f.stdin
	t.Cleanup(func() { newCloudManager, cloudStdin = prevNew, prevIn })
	return f
}

func cloudRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	err := run(append([]string{"cloud"}, append(args, "--name", "byoc-cli", "--state-dir", t.TempDir())...), &out, &errb)
	return out.String() + errb.String(), err
}

func TestCloudCLIAWSWholeFlow(t *testing.T) {
	f := newCloudFixture(t)
	out, err := cloudRun(t, "connect", "aws", "--start-url", "https://d-1234567890.awsapps.com/start", "--sso-region", "us-east-1")
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Confirm the code") || !strings.Contains(out, "Connected aws-"+awsfake.DefaultAccount) {
		t.Fatalf("connect printed:\n%s", out)
	}
	if names := f.keys.Names(); len(names) != 1 || names[0] != "connection/aws-"+awsfake.DefaultAccount {
		t.Fatalf("keychain items %v", names)
	}
	// Without --yes, a "no" creates nothing; the price is shown first.
	f.stdin.WriteString("n\n")
	out, err = cloudRun(t, "create", "--size", "medium", "--auto-stop", "30m")
	if err != nil || !strings.Contains(out, "m7i.4xlarge") || !strings.Contains(out, "/hour") || !strings.Contains(out, "Nothing was created") {
		t.Fatalf("create without confirmation: %v\n%s", err, out)
	}
	if len(f.aws.Live()) != 0 {
		t.Fatal("a refused create made resources")
	}
	out, err = cloudRun(t, "create", "--size", "medium", "--auto-stop", "30m", "--yes")
	if err != nil || !strings.Contains(out, "byoc-cli is running") || !strings.Contains(out, "us-east-1") {
		t.Fatalf("create: %v\n%s", err, out)
	}
	out, err = cloudRun(t, "status", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Machine byoc.Machine `json:"machine"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil || st.Machine.State != byoc.Running || st.Machine.Size != "m7i.4xlarge" {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if out, err = cloudRun(t, "resources"); err != nil || !strings.Contains(out, "instance") {
		t.Fatalf("resources: %v\n%s", err, out)
	}
	if out, err = cloudRun(t, "scale", "--size", "large", "--yes"); err != nil {
		t.Fatalf("scale: %v\n%s", err, out)
	}
	if out, err = cloudRun(t, "stop"); err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	if out, err = cloudRun(t, "start"); err != nil {
		t.Fatalf("start: %v\n%s", err, out)
	}
	if out, err = cloudRun(t, "status"); err != nil || !strings.Contains(out, "m7i.8xlarge") {
		t.Fatalf("status after scale: %v\n%s", err, out)
	}
	if out, err = cloudRun(t, "disconnect", "aws-"+awsfake.DefaultAccount); err == nil {
		t.Fatalf("disconnect with a VM was accepted:\n%s", out)
	}
	if out, err = cloudRun(t, "delete", "--yes"); err != nil || !strings.Contains(out, "nothing tagged cloudburrow-instance=byoc-cli is left") {
		t.Fatalf("delete: %v\n%s", err, out)
	}
	if live := f.aws.Live(); len(live) != 0 {
		t.Fatalf("left in the fake: %+v", live)
	}
	if out, err = cloudRun(t, "disconnect", "aws-"+awsfake.DefaultAccount); err != nil {
		t.Fatalf("disconnect: %v\n%s", err, out)
	}
	if names := f.keys.Names(); len(names) != 0 {
		t.Fatalf("keychain items after disconnect %v", names)
	}
}

func TestCloudCLIAzureAndFailures(t *testing.T) {
	f := newCloudFixture(t)
	out, err := cloudRun(t, "connect", "azure", "--subscription", azurefake.Subscription)
	if err != nil || !strings.Contains(out, "Connected azure-"+azurefake.Subscription) {
		t.Fatalf("connect azure: %v\n%s", err, out)
	}
	// Quota exceeded on the VM: rolled back, nothing left.
	f.az.FailNext("Microsoft.Compute/virtualMachines/write", 409, "OperationNotAllowed", "Operation could not be completed as it results in exceeding approved standardDSv5Family Cores quota.")
	out, err = cloudRun(t, "create", "--region", "eastus", "--yes")
	if err == nil || !strings.Contains(out, "↺") {
		t.Fatalf("create past the quota: %v\n%s", err, out)
	}
	if ids := f.az.IDs(); len(ids) != 0 {
		t.Fatalf("a failed create left %v", ids)
	}
	if out, err = cloudRun(t, "create", "--region", "eastus", "--size", "gpu", "--auto-stop", "0", "--yes"); err != nil {
		t.Fatalf("create gpu: %v\n%s", err, out)
	}
	if out, err = cloudRun(t, "scale", "--size", "small", "--yes"); err == nil || !strings.Contains(err.Error(), "NVIDIA driver") {
		t.Fatalf("scale gpu to cpu: %v\n%s", err, out)
	}
	if out, err = cloudRun(t, "delete", "--yes"); err != nil {
		t.Fatalf("delete: %v\n%s", err, out)
	}
	if _, err := cloudRun(t, "connect", "azure", "--access-key"); err == nil {
		t.Fatal("an Azure access key was accepted")
	}
	if _, err := cloudRun(t, "connect", "aws"); err == nil {
		t.Fatal("connect aws without --start-url was accepted")
	}
	if _, err := cloudRun(t, "bogus"); err == nil {
		t.Fatal("an unknown command was accepted")
	}
}

func TestCloudCLIAccessKey(t *testing.T) {
	f := newCloudFixture(t)
	f.stdin.WriteString("AKIDTESTKEY\ntest-secret\n\n")
	out, err := cloudRun(t, "connect", "aws", "--access-key")
	if err != nil || !strings.Contains(out, "Connected aws-") {
		t.Fatalf("connect --access-key: %v\n%s", err, out)
	}
	if strings.Contains(out, "test-secret") {
		t.Fatal("the secret was printed")
	}
	// A missing permission stops the create before anything is made.
	f.aws.Deny("ec2:RunInstances")
	out, err = cloudRun(t, "create", "--region", "us-east-1", "--yes")
	if err == nil || !strings.Contains(err.Error(), "ec2:RunInstances") {
		t.Fatalf("create without RunInstances: %v\n%s", err, out)
	}
	if len(f.aws.Live()) != 0 {
		t.Fatal("resources were made")
	}
}

func TestCloudHelpAndParseAutoStop(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"cloud", "--help"}, &out, &out); err != nil || !strings.Contains(out.String(), "connect aws") {
		t.Fatalf("help: %v %s", err, out.String())
	}
	for in, want := range map[string]int{"0": 0, "never": 0, "30m": 30, "2h": 120} {
		if got, err := parseAutoStop(in); err != nil || got != want {
			t.Errorf("parseAutoStop(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"10s", "9000h", "soon"} {
		if _, err := parseAutoStop(in); err == nil {
			t.Errorf("parseAutoStop(%q) accepted", in)
		}
	}
}

// TestCloudCLIConnectModesAndSettings: AWS defaults to Session Manager
// with nothing inbound; --connect ssh opens SSH to the user's address,
// which allow-my-ip moves; set changes auto-stop after create.
func TestCloudCLIConnectModesAndSettings(t *testing.T) {
	f := newCloudFixture(t)
	if out, err := cloudRun(t, "connect", "aws", "--start-url", "https://d-1234567890.awsapps.com/start", "--sso-region", "us-east-1"); err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}
	out, err := cloudRun(t, "create", "--yes")
	if err != nil || !strings.Contains(out, "Connect: AWS Systems Manager Session Manager, nothing inbound open") {
		t.Fatalf("create: %v\n%s", err, out)
	}
	for _, r := range f.aws.Live() {
		if r.Kind == "security-group" && len(r.Ingress) != 0 {
			t.Errorf("SSM mode opened %v", r.Ingress)
		}
	}
	if out, err = cloudRun(t, "status"); err != nil || !strings.Contains(out, "Connect: Session Manager; nothing inbound is open") ||
		!strings.Contains(out, "Auto-stop: after 1 hour idle") {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if out, err = cloudRun(t, "allow-my-ip"); err == nil || !strings.Contains(err.Error(), "nothing inbound") {
		t.Fatalf("allow-my-ip on SSM: %v\n%s", err, out)
	}
	// Auto-stop on a stopped VM: recorded, applied at the next start.
	if out, err = cloudRun(t, "stop"); err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	if out, err = cloudRun(t, "set", "byoc-cli", "--auto-stop", "2h"); err != nil || !strings.Contains(out, "auto-stop after 2 hours idle") ||
		!strings.Contains(out, "next starts") {
		t.Fatalf("set: %v\n%s", err, out)
	}
	if out, err = cloudRun(t, "set", "--auto-stop", "soon"); err == nil {
		t.Fatalf("a bad duration was accepted:\n%s", out)
	}
	if out, err = cloudRun(t, "set"); err == nil || !strings.Contains(err.Error(), "--auto-stop") {
		t.Fatalf("set without a setting: %v\n%s", err, out)
	}
	if out, err = cloudRun(t, "delete", "--yes"); err != nil {
		t.Fatalf("delete: %v\n%s", err, out)
	}
	if live := f.aws.Live(); len(live) != 0 {
		t.Fatalf("left in the fake (the role too?): %+v", live)
	}
	// SSH mode: the rule admits the user's address only.
	if out, err = cloudRun(t, "create", "--connect", "ssh", "--yes"); err != nil || !strings.Contains(out, "Connect: SSH") {
		t.Fatalf("create --connect ssh: %v\n%s", err, out)
	}
	var ingress []string
	for _, r := range f.aws.Live() {
		if r.Kind == "security-group" {
			ingress = r.Ingress
		}
	}
	if len(ingress) != 1 || ingress[0] != "tcp 22-22 203.0.113.50/32" {
		t.Fatalf("ingress %v", ingress)
	}
	if out, err = cloudRun(t, "allow-my-ip"); err != nil || !strings.Contains(out, "SSH is open to 203.0.113.50/32 only") {
		t.Fatalf("allow-my-ip: %v\n%s", err, out)
	}
	if out, err = cloudRun(t, "create", "--connect", "telnet", "--yes"); err == nil {
		t.Fatalf("a bad mode was accepted:\n%s", out)
	}
	if out, err = cloudRun(t, "delete", "--yes"); err != nil {
		t.Fatalf("delete: %v\n%s", err, out)
	}
}
