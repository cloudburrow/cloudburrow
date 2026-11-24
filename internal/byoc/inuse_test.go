package byoc_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/byoc/byoctest"
	"github.com/cloudburrow/cloudburrow/internal/byoc/keychain"
)

// A VM an instance runs on holds its cluster and data: Delete refuses it,
// whoever asks (#2209), until the instance moves off it, or the user
// chooses to delete the instance's data with it.
func TestDeleteRefusesAVMAnInstanceRunsOn(t *testing.T) {
	aws := byoctest.New(byoc.AWS)
	user := "lab"
	m, err := byoc.NewManager(byoc.Options{Dir: t.TempDir(), Keychain: keychain.NewMemory(), Drivers: []byoc.Driver{aws},
		IPLookup: func(context.Context) (string, error) { return "203.0.113.9", nil }, OpenBrowser: func(string) error { return nil },
		InUse: func(machine string) string {
			if machine == "vm1" {
				return user
			}
			return ""
		}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, err := m.ConnectKey(ctx, byoc.AWS, byoc.AccessKey{AccessKeyID: "A", SecretAccessKey: "B"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, byoc.CreateRequest{Instance: "vm1", Connection: conn.ID, Region: "us-east-1"}, true, nil); err != nil {
		t.Fatal(err)
	}
	ms, err := m.Machines()
	if err != nil || len(ms) != 1 || ms[0].InUseBy != "lab" {
		t.Fatalf("Machines = %+v, %v; want vm1 in use by lab", ms, err)
	}
	_, err = m.Delete(ctx, "vm1", true, nil)
	var inUse *byoc.InUseError
	if !errors.As(err, &inUse) || !errors.Is(err, byoc.ErrInUse) || inUse.Instance != "lab" {
		t.Fatalf("Delete of a VM lab runs on = %v; want an InUseError", err)
	}
	for _, want := range []string{"cloud move lab --to local", "--with-instance-data", "Delete VM and this instance's data"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lacks %q: %v", want, err)
		}
	}
	if _, err := m.Machine("vm1"); err != nil || len(aws.Resources("vm1")) == 0 {
		t.Fatalf("a refused Delete removed something: %v, live %v", err, aws.Resources("vm1"))
	}
	// Without a release, the forced delete is refused too; a failed
	// release deletes nothing.
	if _, err := m.DeleteWithInstanceData(ctx, "vm1", true, nil, nil); !errors.Is(err, byoc.ErrInUse) {
		t.Fatalf("DeleteWithInstanceData with no release = %v", err)
	}
	if _, err := m.DeleteWithInstanceData(ctx, "vm1", true, nil, func(context.Context, string) error { return errors.New("up did not stop") }); err == nil ||
		!strings.Contains(err.Error(), "up did not stop") || len(aws.Resources("vm1")) == 0 {
		t.Fatalf("a failed release = %v, live %v", err, aws.Resources("vm1"))
	}
	var released string
	if _, err := m.DeleteWithInstanceData(ctx, "vm1", true, nil, func(_ context.Context, u string) error {
		released, user = u, ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if released != "lab" || len(aws.Resources("vm1")) != 0 {
		t.Fatalf("released %q, live %v", released, aws.Resources("vm1"))
	}
	if _, err := m.Machine("vm1"); !errors.Is(err, byoc.ErrNotFound) {
		t.Fatalf("vm1 after its delete: %v", err)
	}
}
