package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// consoleFakeKMS is Cloud KMS with one version per key.
type consoleFakeKMS struct{ version map[string]string }

func (f consoleFakeKMS) Encrypt(_ context.Context, key string, pt []byte) ([]byte, string, error) {
	return pt, key + "/cryptoKeyVersions/" + f.version[key], nil
}

func (consoleFakeKMS) Decrypt(_ context.Context, _ string, ct []byte) ([]byte, error) { return ct, nil }

// Disks' Encryption, Update KMS key and Snapshot schedule, and the
// Snapshot schedules page (#2206): Create a snapshot schedule, the list,
// a disk made with a Cloud KMS key and the schedule, the disk's page, Update
// KMS key rotating it, the schedule's Edit, Detach from disk, Attach to
// disk and Delete.
func TestConsoleComputeDiskKeysAndSchedules(t *testing.T) {
	ctx := context.Background()
	s := computeVMConsoleFixture(t)
	const project = "keys-p"
	const key = "projects/keys-p/locations/us-central1/keyRings/r/cryptoKeys/k"
	kms := consoleFakeKMS{version: map[string]string{key: "1"}}
	s.svc.api.UseKMS(kms)
	disks, scheds := computeDisksProvider{s}, computeSnapshotSchedulesProvider{s}
	if _, err := scheds.Create(ctx, project, map[string]string{"name": "nightly", "region": "us-central1", "frequency": "Hourly",
		"hours": "6", "startTime": "02:00", "retentionDays": "7", "onSourceDiskDelete": "APPLY_RETENTION_POLICY", "labels": `{"k":"v"}`}); err != nil {
		t.Fatal(err)
	}
	if _, err := scheds.Create(ctx, project, map[string]string{"name": "bad", "region": "us-central1", "frequency": "Hourly", "hours": "30"}); err == nil {
		t.Error("an hourly schedule of 30 hours was made")
	}
	l, err := scheds.List(ctx, project)
	if err != nil || len(l.Items) != 1 || l.Items[0].Fields["Frequency"] != "Every 6 hours from 02:00 UTC" || l.Items[0].Fields["Auto-delete snapshots after"] != "7 days" {
		t.Fatalf("Snapshot schedules: %+v %v", l.Items, err)
	}
	if _, err := disks.Create(ctx, project, map[string]string{"name": "enc", "zone": "us-central1-a", "sizeGb": "10", "type": "pd-balanced",
		"encryption": "Cloud KMS key", "kmsKeyName": key, "snapshotSchedule": "nightly"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	path := []string{"us-central1-a", "enc"}
	det, err := disks.Detail(ctx, project, path)
	if err != nil {
		t.Fatal(err)
	}
	props := map[string]string{}
	for _, p := range det.Sections[0].Groups[0].Properties {
		props[p.Label] = p.Value
	}
	if props["Encryption"] != "Customer-managed ("+key+"/cryptoKeyVersions/1)" || props["Resource policies"] != "nightly" {
		t.Errorf("the disk's page: %v", props)
	}
	found := false
	for _, a := range disks.DetailActions(ctx, project, path) {
		found = found || a.ID == "updatekmskey"
	}
	if !found {
		t.Fatal("no Update KMS key on a CMEK disk")
	}
	kms.version[key] = "2"
	if err := disks.ActAt(ctx, project, path, "updatekmskey", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	d, _ := s.call("GET", project, "zones/us-central1-a/disks/enc", nil, nil)
	if got := vpcStr(vmMap(d, "diskEncryptionKey"), "kmsKeyName"); got != key+"/cryptoKeyVersions/2" {
		t.Errorf("after Update KMS key: %s", got)
	}

	sp := []string{"us-central1", "nightly"}
	sd, err := scheds.Detail(ctx, project, sp)
	if err != nil || sd.Sections[1].Listing.Total != 1 {
		t.Fatalf("the schedule's page: %+v %v", sd, err)
	}
	if err := scheds.ActAt(ctx, project, sp, "edit", map[string]string{"frequency": "Weekly", "day": "FRIDAY", "startTime": "03:00",
		"retentionDays": "30", "description": "weekly now"}); err != nil {
		t.Fatal(err)
	}
	pol, _ := s.call("GET", project, "regions/us-central1/resourcePolicies/nightly", nil, nil)
	if scheduleText(pol) != "Weekly: FRIDAY 03:00 UTC" || retentionText(pol) != "30 days" || vpcStr(pol, "description") != "weekly now" {
		t.Errorf("after Edit: %s %s %v", scheduleText(pol), retentionText(pol), pol["description"])
	}
	var del bool
	for _, a := range scheds.DetailActions(ctx, project, sp) {
		if a.ID == "delete" {
			del = a.Unavailable == ""
		}
	}
	if del {
		t.Error("Delete is offered while the schedule is attached")
	}
	base := "zones/us-central1-a/disks/enc"
	if err := scheds.ActAt(ctx, project, sp, "detach", map[string]string{"disk": base}); err != nil {
		t.Fatal(err)
	}
	if err := scheds.ActAt(ctx, project, sp, "attach", map[string]string{"disk": base}); err != nil {
		t.Fatal(err)
	}
	if err := scheds.ActAt(ctx, project, sp, "detach", map[string]string{"disk": base}); err != nil {
		t.Fatal(err)
	}
	if err := scheds.ActAt(ctx, project, sp, "delete", nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, _ := scheds.List(ctx, project)
		if len(l.Items) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the schedule was never deleted")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A daily schedule: daysInCycle 1, sent as a number.
	if _, err := scheds.Create(ctx, project, map[string]string{"name": "daily", "region": "us-central1", "frequency": "Daily", "startTime": "05:00"}); err != nil {
		t.Fatal(err)
	}
	if pol, _ := s.call("GET", project, "regions/us-central1/resourcePolicies/daily", nil, nil); scheduleText(pol) != "Daily at 05:00 UTC" {
		t.Errorf("daily schedule: %s", scheduleText(pol))
	}
	if _, err := disks.Create(ctx, project, map[string]string{"name": "nokey", "zone": "us-central1-a", "encryption": "Cloud KMS key"}); err == nil ||
		!strings.Contains(err.Error(), "kmsKeyName") {
		t.Errorf("Cloud KMS key without a key: %v", err)
	}
}
