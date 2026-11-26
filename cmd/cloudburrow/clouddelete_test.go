package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov"
	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov/awsfake"
	"github.com/cloudburrow/cloudburrow/internal/byoc/keychain"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/remotecompute"
)

// `cloudburrow cloud delete` refuses a VM an instance runs on (#2209): the
// instance moves off first, or --with-instance-data stops it and deletes
// its data with the VM. A VM whose instance of its own name never ran is
// deleted as before.
func TestCloudDeleteRefusesAVMAnInstanceRunsOn(t *testing.T) {
	aws := awsfake.New()
	t.Cleanup(aws.Close)
	state := t.TempDir()
	var m *byoc.Manager
	prevNew := newCloudManager
	newCloudManager = func(cfg config.Config) (*byoc.Manager, error) {
		if m != nil {
			return m, nil
		}
		var err error
		m, err = byoc.NewManager(byoc.Options{Dir: filepath.Join(state, "byoc"), Keychain: keychain.NewMemory(),
			Drivers:     []byoc.Driver{awsprov.NewDriver(aws.Options())},
			IPLookup:    func(context.Context) (string, error) { return "203.0.113.50", nil },
			OpenBrowser: func(string) error { go func() { time.Sleep(20 * time.Millisecond); aws.Approve() }(); return nil },
			Location:    func() (float64, float64, bool) { return 40.7, -74.0, true },
			InUse:       func(machine string) string { return instanceOnMachine(cfg, machine) }})
		return m, err
	}
	t.Cleanup(func() { newCloudManager = prevNew })
	cli := func(args ...string) (string, error) {
		var out, errb bytes.Buffer
		err := run(append([]string{"cloud"}, append(args, "--state-dir", state)...), &out, &errb)
		return out.String() + errb.String(), err
	}
	if out, err := cli("connect", "aws", "--start-url", "https://d-1234567890.awsapps.com/start", "--sso-region", "us-east-1", "--name", "vm1"); err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}
	for _, vm := range []string{"vm1", "vm2"} {
		if out, err := cli("create", "--size", "small", "--yes", "--name", vm); err != nil {
			t.Fatalf("create %s: %v\n%s", vm, err, out)
		}
	}
	// Instance lab runs on vm1: its record names the VM, and it has run.
	lab := filepath.Join(state, "lab")
	if err := os.MkdirAll(lab, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := remotecompute.SaveRecord(lab, remotecompute.Record{Target: remotecompute.Target{User: "ubuntu", Host: "198.51.100.7", Port: 22},
		Provisioned: true, Machine: "vm1", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, "up.log"), []byte("up\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := cli("delete", "--yes", "--name", "vm1")
	var inUse *byoc.InUseError
	if !errors.As(err, &inUse) || inUse.Instance != "lab" || !strings.Contains(err.Error(), "cloud move lab --to local") {
		t.Fatalf("delete of the VM lab runs on = %v\n%s", err, out)
	}
	if _, err := m.Machine("vm1"); err != nil {
		t.Fatalf("a refused delete removed vm1: %v", err)
	}
	// vm2's own instance never ran: nothing runs there.
	if out, err := cli("delete", "--yes", "--name", "vm2"); err != nil || !strings.Contains(out, "nothing tagged cloudburrow-instance=vm2 is left") {
		t.Fatalf("delete vm2: %v\n%s", err, out)
	}
	// The user's explicit choice: the VM, and lab's data on it.
	out, err = cli("delete", "--with-instance-data", "--yes", "--name", "vm1")
	if err != nil || !strings.Contains(out, "lab no longer runs on vm1") || !strings.Contains(out, "nothing tagged cloudburrow-instance=vm1 is left") {
		t.Fatalf("delete --with-instance-data: %v\n%s", err, out)
	}
	if _, had, _ := remotecompute.LoadRecord(lab); had {
		t.Fatal("lab still records vm1 as its compute")
	}
	if live := aws.Live(); len(live) != 0 {
		t.Fatalf("left in AWS: %v", live)
	}
}
