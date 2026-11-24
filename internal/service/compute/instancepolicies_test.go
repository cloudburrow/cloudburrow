package compute

import (
	"sync"
	"testing"
	"time"
)

// TestInstanceSchedules (#2206): an instance schedule's validation, one
// per VM, its start and stop as each falls due (only after the VM was
// attached), its status, and that a policy a VM has can't be deleted.
func TestInstanceSchedules(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 11, 24, 7, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	s, err := New(Options{VMs: fakeVMs{}, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	r := "regions/us-central1/"
	z := "zones/us-central1-a/"
	sched := func(start, stop string) obj {
		return obj{"vmStartSchedule": obj{"schedule": start}, "vmStopSchedule": obj{"schedule": stop}, "timeZone": "Europe/Paris"}
	}
	wantStatus(t, s, 400, "vmStartSchedule, vmStopSchedule or both", "POST", r+"resourcePolicies", obj{"name": "x", "instanceSchedulePolicy": obj{}})
	wantStatus(t, s, 400, "one start and one stop operation per hour", "POST", r+"resourcePolicies",
		obj{"name": "x", "instanceSchedulePolicy": sched("* 8 * * *", "0 18 * * *")})
	wantStatus(t, s, 400, "one start and one stop operation per hour", "POST", r+"resourcePolicies",
		obj{"name": "x", "instanceSchedulePolicy": sched("0,30 8 * * *", "0 18 * * *")})
	wantStatus(t, s, 400, "IANA", "POST", r+"resourcePolicies",
		obj{"name": "x", "instanceSchedulePolicy": obj{"vmStartSchedule": obj{"schedule": "0 8 * * *"}, "timeZone": "Mars/Base"}})
	wantStatus(t, s, 400, "after the start time", "POST", r+"resourcePolicies",
		obj{"name": "x", "instanceSchedulePolicy": obj{"vmStartSchedule": obj{"schedule": "0 8 * * *"},
			"startTime": "2027-01-01T00:00:00Z", "expirationTime": "2026-01-01T00:00:00Z"}})
	// 9:00 and 18:00 in Paris are 8:00 and 17:00 UTC in November.
	mustCall(t, s, "POST", r+"resourcePolicies", obj{"name": "office", "instanceSchedulePolicy": sched("0 9 * * 1-5", "0 18 * * 1-5")})
	mustCall(t, s, "POST", r+"resourcePolicies", obj{"name": "other", "instanceSchedulePolicy": sched("0 7 * * *", "0 19 * * *")})
	newVM(t, s, "us-central1-a", "vm", "e2-small")
	mustCall(t, s, "POST", z+"instances/vm/stop", nil)
	mustCall(t, s, "POST", z+"instances/vm/addResourcePolicies", obj{"resourcePolicies": []any{r + "resourcePolicies/office"}})
	wantStatus(t, s, 400, "Multiple resource policies of the same type", "POST", z+"instances/vm/addResourcePolicies",
		obj{"resourcePolicies": []any{"projects/p/" + r + "resourcePolicies/other"}})
	wantStatus(t, s, 400, "already being used by", "DELETE", r+"resourcePolicies/office", nil)
	vm := mustCall(t, s, "GET", z+"instances/vm", nil)
	if l := strs(vm, "resourcePolicies"); len(l) != 1 {
		t.Fatalf("resourcePolicies = %v", vm["resourcePolicies"])
	}
	step := func(to time.Time) {
		mu.Lock()
		now = to
		mu.Unlock()
		s.mu.Lock()
		s.runInstanceSchedulesLocked(to)
		s.mu.Unlock()
	}
	status := func() string {
		for deadline := time.Now().Add(5 * time.Second); ; {
			s.mu.Lock()
			st := str(s.res["projects/p/zones/us-central1-a/instances/vm"], "status")
			s.mu.Unlock()
			if (st != stProvisioning && st != stStopping) || time.Now().After(deadline) {
				return st
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	step(time.Date(2026, 11, 24, 7, 59, 0, 0, time.UTC))
	if st := status(); st != stTerminated {
		t.Fatalf("before 9:00 Paris: %s", st)
	}
	step(time.Date(2026, 11, 24, 8, 0, 30, 0, time.UTC))
	if st := status(); st != stRunning {
		t.Fatalf("after 9:00 Paris: %s", st)
	}
	step(time.Date(2026, 11, 24, 17, 1, 0, 0, time.UTC))
	if st := status(); st != stTerminated {
		t.Fatalf("after 18:00 Paris: %s", st)
	}
	po := mustCall(t, s, "GET", r+"resourcePolicies/office", nil)
	rs, _ := po["resourceStatus"].(map[string]any)
	isp, _ := rs["instanceSchedulePolicy"].(map[string]any)
	if str(isp, "lastRunStartTime") != "2026-11-24T17:00:00Z" || str(isp, "nextRunStartTime") != "2026-11-25T08:00:00Z" {
		t.Errorf("resourceStatus = %v", rs)
	}
	mustCall(t, s, "POST", z+"instances/vm/removeResourcePolicies", obj{"resourcePolicies": []any{r + "resourcePolicies/office"}})
	mustCall(t, s, "DELETE", r+"resourcePolicies/office", nil)
}

// TestPlacementPolicies (#2206): spread and compact policies' rules, the
// availability domain a spread policy gives, a compact policy's machine
// series, stopped VM and topology, and workload policies.
func TestPlacementPolicies(t *testing.T) {
	s, err := New(Options{VMs: fakeVMs{}})
	if err != nil {
		t.Fatal(err)
	}
	r := "regions/us-central1/"
	z := "zones/us-central1-a/"
	gp := func(name string, p obj) obj { return obj{"name": name, "groupPlacementPolicy": p} }
	wantStatus(t, s, 400, "between 1 and 8", "POST", r+"resourcePolicies", gp("x", obj{"availabilityDomainCount": 9}))
	wantStatus(t, s, 400, "not on compact", "POST", r+"resourcePolicies", gp("x", obj{"collocation": "COLLOCATED", "availabilityDomainCount": 2}))
	wantStatus(t, s, 400, "1, 2 or 3", "POST", r+"resourcePolicies", gp("x", obj{"collocation": "COLLOCATED", "maxDistance": 4}))
	wantStatus(t, s, 400, "from 2 to 22", "POST", r+"resourcePolicies", gp("x", obj{"collocation": "COLLOCATED", "maxDistance": 1, "vmCount": 23}))
	mustCall(t, s, "POST", r+"resourcePolicies", gp("spread", obj{"availabilityDomainCount": 2}))
	mustCall(t, s, "POST", r+"resourcePolicies", gp("compact", obj{"collocation": "COLLOCATED", "maxDistance": 2}))
	wantStatus(t, s, 400, "HIGH_AVAILABILITY or HIGH_THROUGHPUT", "POST", r+"resourcePolicies", obj{"name": "w", "workloadPolicy": obj{"type": "FAST"}})
	wantStatus(t, s, 400, "HIGH_THROUGHPUT only", "POST", r+"resourcePolicies", obj{"name": "w", "workloadPolicy": obj{"type": "HIGH_AVAILABILITY", "maxTopologyDistance": "BLOCK"}})
	mustCall(t, s, "POST", r+"resourcePolicies", obj{"name": "w", "workloadPolicy": obj{"type": "HIGH_THROUGHPUT", "maxTopologyDistance": "BLOCK"}})

	domains := map[int64]int{}
	for _, n := range []string{"a", "b", "c", "d"} {
		newVM(t, s, "us-central1-a", n, "e2-small")
		mustCall(t, s, "POST", z+"instances/"+n+"/addResourcePolicies", obj{"resourcePolicies": []any{r + "resourcePolicies/spread"}})
		vm := mustCall(t, s, "GET", z+"instances/"+n, nil)
		d, _, _ := intField(vm["scheduling"].(map[string]any)["availabilityDomain"], "")
		domains[d]++
	}
	if domains[1] != 2 || domains[2] != 2 {
		t.Errorf("availability domains = %v", domains)
	}
	wantStatus(t, s, 400, "managed instance group", "POST", z+"instances/a/addResourcePolicies", obj{"resourcePolicies": []any{r + "resourcePolicies/w"}})

	newVM(t, s, "us-central1-a", "e", "e2-small")
	mustCall(t, s, "POST", z+"instances/e/stop", nil)
	wantStatus(t, s, 400, "aren't supported for machine type 'e2-small'", "POST", z+"instances/e/addResourcePolicies", obj{"resourcePolicies": []any{r + "resourcePolicies/compact"}})
	newVM(t, s, "us-central1-a", "n", "n2-standard-2")
	wantStatus(t, s, 400, "stop the instance", "POST", z+"instances/n/addResourcePolicies", obj{"resourcePolicies": []any{r + "resourcePolicies/compact"}})
	mustCall(t, s, "POST", z+"instances/n/stop", nil)
	wantStatus(t, s, 400, "don't support live migration", "POST", z+"instances/n/addResourcePolicies", obj{"resourcePolicies": []any{r + "resourcePolicies/compact"}})
	s.mu.Lock()
	s.res["projects/p/zones/us-central1-a/instances/n"]["scheduling"].(map[string]any)["onHostMaintenance"] = "TERMINATE"
	s.mu.Unlock()
	mustCall(t, s, "POST", z+"instances/n/addResourcePolicies", obj{"resourcePolicies": []any{r + "resourcePolicies/compact"}})
	vm := mustCall(t, s, "GET", z+"instances/n", nil)
	topo, _ := vm["resourceStatus"].(map[string]any)["physicalHostTopology"].(map[string]any)
	if str(topo, "cluster") == "" || str(topo, "block") == "" || str(topo, "subblock") != "" {
		t.Errorf("physicalHostTopology = %v", topo)
	}
	mustCall(t, s, "POST", z+"instances/n/removeResourcePolicies", obj{"resourcePolicies": []any{r + "resourcePolicies/compact"}})
	if vm := mustCall(t, s, "GET", z+"instances/n", nil); vm["resourceStatus"] != nil {
		t.Errorf("after remove: %v", vm["resourceStatus"])
	}
}
