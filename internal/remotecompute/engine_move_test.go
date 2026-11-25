package remotecompute

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
)

func TestEngineLeavesAnInstanceOnThisMachineToMove(t *testing.T) {
	e := &Engine{StateDir: t.TempDir(), LocalState: func(instance string) bool { return instance == "lab" }}
	err := e.Connect(context.Background(), byoc.RemoteTarget{Instance: "lab", Host: "192.0.2.7", User: "ubuntu", PrivateKey: []byte("k"), HostKey: "ssh-ed25519 AAAA"})
	if !errors.Is(err, byoc.ErrMoveNeeded) {
		t.Fatalf("Connect = %v, want ErrMoveNeeded", err)
	}
	if _, had, _ := LoadRecord(e.StateDir + "/lab"); had {
		t.Fatal("the instance's compute was recorded as the VM while its cluster is here")
	}
}

func TestRecordMachineName(t *testing.T) {
	if got := (Record{}).MachineName("lab"); got != "lab" {
		t.Fatal(got)
	}
	if got := (Record{Machine: "lab-vm"}).MachineName("lab"); got != "lab-vm" {
		t.Fatal(got)
	}
}
