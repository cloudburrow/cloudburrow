package compute

import (
	"strings"
	"testing"
	"time"
)

// TestDiskLicensesAndGuestOsFeatures (#2206): disks.update and
// regionDisks.update of licenses, licenseCodes, userLicenses and
// guestOsFeatures.
func TestDiskLicensesAndGuestOsFeatures(t *testing.T) {
	s, err := New(Options{VMs: newDataVMs()})
	if err != nil {
		t.Fatal(err)
	}
	z := "zones/us-central1-a/"
	const ubuntu = "projects/ubuntu-os-cloud/global/licenses/ubuntu-2404-lts"
	const pro = "projects/ubuntu-os-pro-cloud/global/licenses/ubuntu-pro-2404-lts"
	const debian = "projects/debian-cloud/global/licenses/debian-12"
	mustCall(t, s, "POST", z+"disks", obj{"name": "u", "sourceImage": "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts"})
	mustCall(t, s, "POST", z+"disks", obj{"name": "d", "sourceImage": "projects/debian-cloud/global/images/family/debian-12"})
	u := mustCall(t, s, "GET", z+"disks/u", nil)
	if l := strs(u, "licenseCodes"); len(l) != 1 || l[0] != licenseCode(ubuntu) || len(u["guestOsFeatures"].([]any)) != 3 {
		t.Fatalf("a disk from the Ubuntu image: %v %v", u["licenseCodes"], u["guestOsFeatures"])
	}
	// Ubuntu to Ubuntu Pro: append, then replace.
	if _, err := patchCall(t, s, z+"disks/u", "licenses", obj{"licenses": []any{ubuntu, "https://www.googleapis.com/compute/v1/" + pro}}); err != nil {
		t.Fatal(err)
	}
	if _, err := patchCall(t, s, z+"disks/u", "licenseCodes", obj{"licenseCodes": []any{licenseCode(pro)}}); err != nil {
		t.Fatal(err)
	}
	u = mustCall(t, s, "GET", z+"disks/u", nil)
	if l := strs(u, "licenses"); len(l) != 1 || !strings.HasSuffix(l[0], pro) {
		t.Errorf("after the replacement: %v", u["licenses"])
	}
	// Debian's license is not eligible.
	if _, err := patchCall(t, s, z+"disks/d", "licenses", obj{"licenses": []any{}}); ErrorStatus(err) != 400 || !strings.Contains(err.Error(), "eligible") {
		t.Errorf("removing Debian's license: %v", err)
	}
	if _, err := patchCall(t, s, z+"disks/d", "licenses", obj{"licenses": []any{debian, pro}}); err != nil {
		t.Errorf("appending Ubuntu Pro to Debian's: %v", err)
	}
	if _, err := patchCall(t, s, z+"disks/d", "licenseCodes", obj{"licenseCodes": []any{"12"}}); ErrorStatus(err) != 400 {
		t.Errorf("an unknown code: %v", err)
	}
	if _, err := patchCall(t, s, z+"disks/d", "licenses,licenseCodes", obj{}); ErrorStatus(err) != 400 {
		t.Errorf("both: %v", err)
	}
	// A disk of a running instance keeps its licenses.
	newVM(t, s, "us-central1-a", "vm", "n2-standard-2")
	mustCall(t, s, "POST", z+"instances/vm/attachDisk", obj{"source": z + "disks/u"})
	if _, err := patchCall(t, s, z+"disks/u", "licenses", obj{"licenses": []any{ubuntu}}); ErrorStatus(err) != 400 || !strings.Contains(err.Error(), "stop the instance") {
		t.Errorf("a running instance's disk: %v", err)
	}
	mustCall(t, s, "POST", z+"instances/vm/stop", nil)
	if _, err := patchCall(t, s, z+"disks/u", "licenses", obj{"licenses": []any{ubuntu}}); err != nil {
		t.Errorf("a stopped instance's disk: %v", err)
	}

	// User licenses and guest OS features, on a regional disk too.
	r := "regions/us-central1/"
	mustCall(t, s, "POST", r+"disks", obj{"name": "rd", "sizeGb": "10", "replicaZones": []any{"zones/us-central1-a", "zones/us-central1-b"}})
	if _, err := patchCall(t, s, r+"disks/rd", "userLicenses,guestOsFeatures", obj{"userLicenses": []any{"projects/p/global/licenses/mine"},
		"guestOsFeatures": []any{obj{"type": "GVNIC"}, obj{"type": "GVNIC"}}}); err != nil {
		t.Fatal(err)
	}
	rd := mustCall(t, s, "GET", r+"disks/rd", nil)
	if l := strs(rd, "userLicenses"); len(l) != 1 || len(rd["guestOsFeatures"].([]any)) != 1 {
		t.Errorf("regional disk: %v %v", rd["userLicenses"], rd["guestOsFeatures"])
	}
	if _, err := patchCall(t, s, r+"disks/rd", "guestOsFeatures", obj{"guestOsFeatures": []any{obj{"type": "NOPE"}}}); ErrorStatus(err) != 400 {
		t.Errorf("an unknown feature: %v", err)
	}
	if _, err := patchCall(t, s, r+"disks/rd", "user_licenses", obj{}); err != nil {
		t.Fatal(err)
	}
	if rd := mustCall(t, s, "GET", r+"disks/rd", nil); rd["userLicenses"] != nil {
		t.Errorf("user licenses cleared: %v", rd["userLicenses"])
	}
}

// TestSnapshotSchedules (#2206): snapshot schedule resource policies,
// their limits, patch, attachment to disks, the snapshots they take and
// their retention.
func TestSnapshotSchedules(t *testing.T) {
	now := time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC)
	s, err := New(Options{VMs: newDataVMs(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	r := "regions/us-central1/"
	z := "zones/us-central1-a/"
	hourly := func(n any, start string) obj {
		return obj{"schedule": obj{"hourlySchedule": obj{"hoursInCycle": n, "startTime": start}}}
	}
	for _, c := range []struct {
		sp   obj
		want string
	}{
		{hourly(24.0, "10:00"), "between 1 and 23"},
		{hourly(4.0, "10:13"), "must start on the hour"},
		{obj{"schedule": obj{"dailySchedule": obj{"daysInCycle": 2.0, "startTime": "10:00"}}}, "daysInCycle must be 1"},
		{obj{"schedule": obj{"dailySchedule": obj{"daysInCycle": 1.0, "startTime": "10:00"}, "hourlySchedule": obj{}}}, "exactly one"},
		{obj{"schedule": obj{"weeklySchedule": obj{"dayOfWeeks": []any{obj{"day": "FUNDAY", "startTime": "10:00"}}}}}, "FUNDAY"},
		{obj{"schedule": obj{"dailySchedule": obj{"daysInCycle": 1.0, "startTime": "10:00"}}, "retentionPolicy": obj{"maxRetentionDays": 0.0}}, "1 or higher"},
	} {
		wantStatus(t, s, 400, c.want, "POST", r+"resourcePolicies", obj{"name": "bad", "snapshotSchedulePolicy": c.sp})
	}
	sp := hourly(4.0, "10:00")
	sp["retentionPolicy"] = obj{"maxRetentionDays": 1.0, "onSourceDiskDelete": "APPLY_RETENTION_POLICY"}
	sp["snapshotProperties"] = obj{"labels": obj{"k": "v"}}
	mustCall(t, s, "POST", r+"resourcePolicies", obj{"name": "every4h", "snapshotSchedulePolicy": sp})
	po := mustCall(t, s, "GET", r+"resourcePolicies/every4h", nil)
	if rp := po["snapshotSchedulePolicy"].(map[string]any)["retentionPolicy"].(map[string]any); rp["onSourceDiskDelete"] != "APPLY_RETENTION_POLICY" {
		t.Errorf("policy: %v", po)
	}
	mustCall(t, s, "POST", "regions/us-east1/resourcePolicies", obj{"name": "east", "snapshotSchedulePolicy": hourly(1.0, "00:00")})
	mustCall(t, s, "POST", z+"disks", obj{"name": "a", "sizeGb": "10", "resourcePolicies": []any{r + "resourcePolicies/every4h"}})
	wantStatus(t, s, 400, "region", "POST", z+"disks/a/addResourcePolicies", obj{"resourcePolicies": []any{"regions/us-east1/resourcePolicies/east"}})
	wantStatus(t, s, 400, "already has", "POST", z+"disks/a/addResourcePolicies", obj{"resourcePolicies": []any{"every4h"}})
	wantStatus(t, s, 400, "already being used", "DELETE", r+"resourcePolicies/every4h", nil)
	// Ten schedules at most.
	var many []any
	for i := range 10 {
		n := "s" + string(rune('a'+i))
		mustCall(t, s, "POST", r+"resourcePolicies", obj{"name": n, "snapshotSchedulePolicy": hourly(1.0, "00:00")})
		many = append(many, n)
	}
	wantStatus(t, s, 400, "maximum of 10", "POST", z+"disks/a/addResourcePolicies", obj{"resourcePolicies": many})
	mustCall(t, s, "POST", z+"disks/a/addResourcePolicies", obj{"resourcePolicies": many[:9]})
	mustCall(t, s, "POST", z+"disks/a/removeResourcePolicies", obj{"resourcePolicies": many[:9]})

	// Patch: the schedule and retention, not the snapshot properties.
	if _, err := s.Call("PATCH", "p", r+"resourcePolicies/every4h", map[string][]string{"updateMask": {"snapshotSchedulePolicy.snapshotProperties"}}, obj{}); ErrorStatus(err) != 400 {
		t.Errorf("patch of snapshotProperties: %v", err)
	}
	mustCall(t, s, "PATCH", r+"resourcePolicies/every4h", obj{"description": "d", "snapshotSchedulePolicy": hourly(4.0, "02:00")})
	if po := mustCall(t, s, "GET", r+"resourcePolicies/every4h", nil); po["description"] != "d" {
		t.Errorf("after patch: %v", po)
	}

	// 09:30 is before the first 4-hourly slot after creation (10:00);
	// 10:30 is in it.
	snaps := func() []obj {
		var out []obj
		for k, o := range s.res {
			if strings.Contains(k, "/global/snapshots/") {
				out = append(out, o)
			}
		}
		return out
	}
	s.mu.Lock()
	s.runSchedulesLocked()
	s.mu.Unlock()
	if n := len(snaps()); n != 0 {
		t.Fatalf("%d snapshots before the slot", n)
	}
	now = now.Add(time.Hour)
	for range 2 {
		s.mu.Lock()
		s.runSchedulesLocked()
		s.mu.Unlock()
		waitAllOps(t, s)
	}
	l := snaps()
	if len(l) != 1 {
		t.Fatalf("%d snapshots in the slot", len(l))
	}
	sn := l[0]
	if !strings.HasPrefix(str(sn, "name"), "a-us-central1-a-20261124103000-") || sn["autoCreated"] != true ||
		!strings.HasSuffix(str(sn, "sourceSnapshotSchedulePolicy"), "resourcePolicies/every4h") || sn["labels"] == nil {
		t.Errorf("scheduled snapshot: %v", sn)
	}
	// The next slot (14:00) takes another; a day later the first expires,
	// a newer one existing.
	now = now.Add(4 * time.Hour)
	s.mu.Lock()
	s.runSchedulesLocked()
	s.mu.Unlock()
	waitAllOps(t, s)
	if n := len(snaps()); n != 2 {
		t.Fatalf("%d snapshots after the second slot", n)
	}
	mustCall(t, s, "POST", z+"disks/a/removeResourcePolicies", obj{"resourcePolicies": []any{"every4h"}})
	now = now.Add(25 * time.Hour)
	s.mu.Lock()
	s.runSchedulesLocked()
	s.mu.Unlock()
	waitAllOps(t, s)
	if n := len(snaps()); n != 1 {
		t.Errorf("%d snapshots after retention, want the newest kept", n)
	}
}

func waitAllOps(t *testing.T, s *Server) {
	t.Helper()
	s.mu.Lock()
	var l []string
	for k := range s.waiters {
		l = append(l, k)
	}
	s.mu.Unlock()
	for _, k := range l {
		s.waitOp(t.Context(), k)
	}
}
