package compute

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func patchCall(t *testing.T, s *Server, path, paths string, body obj) (obj, error) {
	t.Helper()
	out, err := s.Call("PATCH", "p", path, url.Values{"paths": {paths}}, body)
	if err == nil && out["status"] != "DONE" {
		s.waitOp(t.Context(), str(out, "selfLink"))
	}
	return out, err
}

// TestHyperdiskBalancedHA (#2206): the documented size, IOPS and
// throughput limits and defaults of Hyperdisk Balanced High Availability,
// its access modes, its machine series and multi-writer's rules.
func TestHyperdiskBalancedHA(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	s, err := New(Options{VMs: newDataVMs(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	r := "regions/us-central1/"
	zs := []any{"zones/us-central1-a", "zones/us-central1-b"}
	ha := r + "diskTypes/" + hdbHA
	for _, c := range []struct {
		body obj
		want string
	}{
		{obj{"name": "h", "sizeGb": "3", "type": ha, "replicaZones": zs}, "between 4 GB"},
		{obj{"name": "h", "type": ha, "replicaZones": zs, "provisionedIops": "4000"}, "without setting a size"},
		{obj{"name": "h", "sizeGb": "10", "type": ha, "replicaZones": zs, "provisionedIops": "6000"}, "between 3000 and 5000"},
		{obj{"name": "h", "sizeGb": "4", "type": ha, "replicaZones": zs, "provisionedIops": "3000"}, "between 2000 and 2000"},
		{obj{"name": "h", "sizeGb": "500", "type": ha, "replicaZones": zs, "provisionedIops": "100001"}, "between 3000 and 100000"},
		{obj{"name": "h", "sizeGb": "500", "type": ha, "replicaZones": zs, "provisionedIops": "4000", "provisionedThroughput": "1001"}, "between 140 and 1000"},
		{obj{"name": "h", "sizeGb": "500", "type": ha, "replicaZones": zs, "provisionedIops": "100000", "provisionedThroughput": "300"}, "between 391 and 2400"},
		{obj{"name": "h", "sizeGb": "10", "type": ha, "replicaZones": zs, "accessMode": roMany}, "READ_WRITE_MANY) access modes only"},
		{obj{"name": "h", "sizeGb": "10", "replicaZones": zs, "provisionedIops": "3000"}, "can't be set for disk type pd-balanced"},
	} {
		wantStatus(t, s, 400, c.want, "POST", r+"disks", c.body)
	}
	for _, c := range []struct {
		size             string
		iops, tput, mode string
	}{{"4", "2000", "140", rwSingle}, {"100", "3600", "290", rwSingle}, {"", "3600", "290", rwSingle}, {"20000", "100000", "2400", rwSingle}} {
		b := obj{"name": "d" + c.size, "type": ha, "replicaZones": zs}
		if c.size != "" {
			b["sizeGb"] = c.size
		}
		mustCall(t, s, "POST", r+"disks", b)
		d := mustCall(t, s, "GET", r+"disks/d"+c.size, nil)
		if d["provisionedIops"] != c.iops || d["provisionedThroughput"] != c.tput || d["accessMode"] != c.mode {
			t.Errorf("defaults of a %s GiB volume: %v %v %v", c.size, d["provisionedIops"], d["provisionedThroughput"], d["accessMode"])
		}
	}

	// Machine series: E2 is refused, C3 attaches it.
	newVM(t, s, "us-central1-a", "e2vm", "e2-standard-2")
	newVM(t, s, "us-central1-a", "c3a", "c3-standard-4")
	newVM(t, s, "us-central1-b", "c3b", "c3-standard-4")
	newVM(t, s, "us-central1-a", "n2vm", "n2-standard-2")
	newVM(t, s, "us-central1-a", "c3c", "c3-standard-4")
	mustCall(t, s, "POST", r+"disks", obj{"name": "mw", "sizeGb": "100", "type": ha, "replicaZones": zs})
	wantStatus(t, s, 400, "cannot be used by [e2-standard-2] machine type", "POST", "zones/us-central1-a/instances/e2vm/attachDisk", obj{"source": r + "disks/mw"})
	mustCall(t, s, "POST", "zones/us-central1-a/instances/c3a/attachDisk", obj{"source": r + "disks/mw"})
	wantStatus(t, s, 400, "already being used", "POST", "zones/us-central1-b/instances/c3b/attachDisk", obj{"source": r + "disks/mw"})

	// The access mode changes only while detached; then two writers.
	if _, err := patchCall(t, s, r+"disks/mw", "accessMode", obj{"accessMode": rwMany}); err == nil || !strings.Contains(err.Error(), "detach the disk from all instances") {
		t.Errorf("accessMode while attached: %v", err)
	}
	if out, err := s.Call("POST", "p", "zones/us-central1-a/instances/c3a/detachDisk", url.Values{"deviceName": {"mw"}}, nil); err != nil {
		t.Fatal(err)
	} else {
		s.waitOp(t.Context(), str(out, "selfLink"))
	}
	if _, err := patchCall(t, s, r+"disks/mw", "accessMode", obj{"accessMode": rwMany}); err != nil {
		t.Fatalf("accessMode: %v", err)
	}
	if d := mustCall(t, s, "GET", r+"disks/mw", nil); d["accessMode"] != rwMany {
		t.Fatalf("accessMode after update: %v", d["accessMode"])
	}
	mustCall(t, s, "POST", "zones/us-central1-a/instances/c3a/attachDisk", obj{"source": r + "disks/mw"})
	mustCall(t, s, "POST", "zones/us-central1-b/instances/c3b/attachDisk", obj{"source": r + "disks/mw"})
	if d := mustCall(t, s, "GET", r+"disks/mw", nil); len(strs(d, "users")) != 2 {
		t.Errorf("multi-writer users: %v", d["users"])
	}
	wantStatus(t, s, 400, "cannot be used by [n2-standard-2]", "POST", "zones/us-central1-a/instances/n2vm/attachDisk", obj{"source": r + "disks/mw"})
	wantStatus(t, s, 400, "Auto-delete can't be enabled", "POST", "zones/us-central1-a/instances/c3c/attachDisk", obj{"source": r + "disks/mw", "autoDelete": true})
	d := s.res["projects/p/regions/us-central1/disks/mw"]
	d["users"] = toAny([]string{"a", "b", "c", "d", "e", "f", "g", "h"})
	wantStatus(t, s, 400, "at most 8 instances", "POST", "zones/us-central1-a/instances/c3c/attachDisk", obj{"source": r + "disks/mw"})
}

// TestDiskUpdate (#2206): disks.update and regionDisks.update, by update
// mask, with the documented change limits.
func TestDiskUpdate(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	s, err := New(Options{VMs: newDataVMs(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	r := "regions/us-central1/"
	zs := []any{"zones/us-central1-a", "zones/us-central1-b"}
	mustCall(t, s, "POST", r+"disks", obj{"name": "h", "sizeGb": "10", "type": r + "diskTypes/" + hdbHA, "replicaZones": zs})
	wantStatus(t, s, 400, "at least one field", "PATCH", r+"disks/h", obj{"provisionedIops": "4000"})
	if _, err := patchCall(t, s, r+"disks/h", "name", obj{"name": "x"}); ErrorStatus(err) != 400 {
		t.Errorf("update of name: %v", err)
	}
	if _, err := patchCall(t, s, r+"disks/h", "provisionedIops", obj{"provisionedIops": "6000"}); ErrorStatus(err) != 400 {
		t.Errorf("IOPS above 500 per GiB: %v", err)
	}
	op, err := patchCall(t, s, r+"disks/h", "provisionedIops,provisionedThroughput", obj{"provisionedIops": "5000", "provisionedThroughput": "500"})
	if err != nil || op["operationType"] != "update" {
		t.Fatalf("update: %v %v", op, err)
	}
	d := mustCall(t, s, "GET", r+"disks/h", nil)
	if d["provisionedIops"] != "5000" || d["provisionedThroughput"] != "500" {
		t.Fatalf("after update: %v", d)
	}
	if _, err := patchCall(t, s, r+"disks/h", "provisionedIops", obj{"provisionedIops": "4000"}); err == nil || !strings.Contains(err.Error(), "once every 4 hours") {
		t.Errorf("second performance change within 4 hours: %v", err)
	}
	now = now.Add(4 * time.Hour)
	if _, err := patchCall(t, s, r+"disks/h", "provisionedIops", obj{"provisionedIops": "4000"}); err != nil {
		t.Errorf("performance change after 4 hours: %v", err)
	}
	// Size: grows only, at most twice in 4 hours (resize counts).
	if _, err := patchCall(t, s, r+"disks/h", "sizeGb", obj{"sizeGb": "8"}); ErrorStatus(err) != 400 {
		t.Errorf("shrink: %v", err)
	}
	if _, err := patchCall(t, s, r+"disks/h", "sizeGb", obj{"sizeGb": "20"}); err != nil {
		t.Fatal(err)
	}
	mustCall(t, s, "POST", r+"disks/h/resize", obj{"sizeGb": "30"})
	if _, err := patchCall(t, s, r+"disks/h", "sizeGb", obj{"sizeGb": "40"}); err == nil || !strings.Contains(err.Error(), "twice in a 4-hour window") {
		t.Errorf("third size change: %v", err)
	}
	if d := mustCall(t, s, "GET", r+"disks/h", nil); d["sizeGb"] != "30" {
		t.Errorf("size: %v", d["sizeGb"])
	}
	// A Persistent Disk takes no performance or access mode.
	mustCall(t, s, "POST", "zones/us-central1-a/disks", obj{"name": "z", "sizeGb": "10"})
	if _, err := patchCall(t, s, "zones/us-central1-a/disks/z", "provisionedIops", obj{"provisionedIops": "3000"}); ErrorStatus(err) != 400 {
		t.Errorf("IOPS of pd-balanced: %v", err)
	}
	if _, err := patchCall(t, s, "zones/us-central1-a/disks/z", "accessMode", obj{"accessMode": rwMany}); ErrorStatus(err) != 400 {
		t.Errorf("accessMode of pd-balanced: %v", err)
	}
	if _, err := patchCall(t, s, "zones/us-central1-a/disks/z", "sizeGb,architecture", obj{"sizeGb": "12", "architecture": "ARM64"}); err != nil {
		t.Fatal(err)
	}
	if z := mustCall(t, s, "GET", "zones/us-central1-a/disks/z", nil); z["sizeGb"] != "12" || z["architecture"] != "ARM64" {
		t.Errorf("zonal update: %v", z)
	}
	if _, err := patchCall(t, s, "zones/us-central1-a/disks/z", "licenses", obj{}); ErrorStatus(err) != 501 {
		t.Errorf("licenses: %v", err)
	}
}
