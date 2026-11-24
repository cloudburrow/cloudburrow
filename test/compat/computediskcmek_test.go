//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// diskKeys makes a key ring in each location, with one ENCRYPT_DECRYPT
// CryptoKey, "disk", in each, through the official Cloud KMS client: the
// keys by location, and a function that sets a version's state.
func diskKeys(t *testing.T, h *Harness, ring string, locations ...string) (map[string]string, func(version string, st kmspb.CryptoKeyVersion_CryptoKeyVersionState)) {
	t.Helper()
	kc := kmsClients(t, h)["grpc"]
	ctx := h.Context()
	keys := map[string]string{}
	for _, loc := range locations {
		r, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + h.Project() + "/locations/" + loc, KeyRingId: ring})
		if err != nil {
			t.Fatal(err)
		}
		ck, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: r.GetName(), CryptoKeyId: "disk",
			CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
		if err != nil {
			t.Fatal(err)
		}
		keys[loc] = ck.GetName()
	}
	return keys, func(version string, st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
		t.Helper()
		if _, err := kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: version, State: st},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
			t.Fatalf("UpdateCryptoKeyVersion %s %v: %v", version, st, err)
		}
	}
}

// newPrimaryVersion makes a new version of key its primary.
func newPrimaryVersion(t *testing.T, h *Harness, key string) string {
	t.Helper()
	kc := kmsClients(t, h)["grpc"]
	v, err := kc.CreateCryptoKeyVersion(h.Context(), &kmspb.CreateCryptoKeyVersionRequest{Parent: key})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kc.UpdateCryptoKeyPrimaryVersion(h.Context(), &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: key,
		CryptoKeyVersionId: v.GetName()[len(key+"/cryptoKeyVersions/"):]}); err != nil {
		t.Fatal(err)
	}
	return v.GetName()
}

// TestComputeDiskCMEKThroughTheGoClient (#2206): zonal and regional disks
// with a customer-managed encryption key of this instance's Cloud KMS, the
// key's location rules, disks.updateKmsKey and regionDisks.updateKmsKey
// rotating to the primary version and changing the key, and a disabled
// version refusing the rotation and the disk's snapshot, through the
// official Go clients.
// covers: compute.disks.updateKmsKey, compute.regionDisks.updateKmsKey, compute.disks.insert, compute.regionDisks.insert
func TestComputeDiskCMEKThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	if os.Getenv(EnvKMS) == "" {
		t.Skip("needs --services compute,kms")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	disks, err := computeapi.NewDisksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	rdisks, _ := computeapi.NewRegionDisksRESTClient(ctx, opts...)
	p := h.Project()
	const region, za = "us-central1", "us-central1-a"
	sfx := fmt.Sprint(time.Now().UnixNano() % 1e6)
	keys, setState := diskKeys(t, h, "dk-"+sfx, region, "global", "europe-west1")
	zd, rd := "cmek-"+sfx, "rcmek-"+sfx
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		if op, err := disks.Delete(c, &computepb.DeleteDiskRequest{Project: p, Zone: za, Disk: zd}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := rdisks.Delete(c, &computepb.DeleteRegionDiskRequest{Project: p, Region: region, Disk: rd}); err == nil {
			_ = op.Wait(c)
		}
	})
	enc := func(k string) *computepb.CustomerEncryptionKey {
		return &computepb.CustomerEncryptionKey{KmsKeyName: proto.String(k)}
	}
	if _, err := disks.Insert(ctx, &computepb.InsertDiskRequest{Project: p, Zone: za, DiskResource: &computepb.Disk{
		Name: proto.String(zd), SizeGb: proto.Int64(10), DiskEncryptionKey: enc(keys["europe-west1"])}}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("a key in another region: %v, want 400", err)
	}
	wait(disks.Insert(ctx, &computepb.InsertDiskRequest{Project: p, Zone: za, DiskResource: &computepb.Disk{
		Name: proto.String(zd), SizeGb: proto.Int64(10), DiskEncryptionKey: enc(keys[region])}}))
	wait(rdisks.Insert(ctx, &computepb.InsertRegionDiskRequest{Project: p, Region: region, DiskResource: &computepb.Disk{
		Name: proto.String(rd), SizeGb: proto.Int64(10), ReplicaZones: []string{"zones/" + za, "zones/us-central1-b"},
		DiskEncryptionKey: enc(keys["global"])}}))
	d, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: p, Zone: za, Disk: zd})
	if err != nil || d.GetDiskEncryptionKey().GetKmsKeyName() != keys[region]+"/cryptoKeyVersions/1" {
		t.Fatalf("the zonal disk's key: %v %v", d.GetDiskEncryptionKey(), err)
	}

	// Rotation to the new primary version, and a change of key.
	v2 := newPrimaryVersion(t, h, keys[region])
	wait(disks.UpdateKmsKey(ctx, &computepb.UpdateKmsKeyDiskRequest{Project: p, Zone: za, Disk: zd,
		DiskUpdateKmsKeyRequestResource: &computepb.DiskUpdateKmsKeyRequest{}}))
	if d, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: p, Zone: za, Disk: zd}); err != nil || d.GetDiskEncryptionKey().GetKmsKeyName() != v2 {
		t.Errorf("after the rotation: %v %v, want %s", d.GetDiskEncryptionKey(), err, v2)
	}
	wait(rdisks.UpdateKmsKey(ctx, &computepb.UpdateKmsKeyRegionDiskRequest{Project: p, Region: region, Disk: rd,
		RegionDiskUpdateKmsKeyRequestResource: &computepb.RegionDiskUpdateKmsKeyRequest{KmsKeyName: proto.String(keys[region])}}))
	if d, err := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: p, Region: region, Disk: rd}); err != nil || d.GetDiskEncryptionKey().GetKmsKeyName() != v2 {
		t.Errorf("the regional disk after the change: %v %v, want %s", d.GetDiskEncryptionKey(), err, v2)
	}

	// A disabled version: the rotation and the snapshot are refused.
	setState(v2, kmspb.CryptoKeyVersion_DISABLED)
	defer setState(v2, kmspb.CryptoKeyVersion_ENABLED)
	if _, err := disks.UpdateKmsKey(ctx, &computepb.UpdateKmsKeyDiskRequest{Project: p, Zone: za, Disk: zd,
		DiskUpdateKmsKeyRequestResource: &computepb.DiskUpdateKmsKeyRequest{}}); computeCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "Cloud KMS error") {
		t.Errorf("rotation with a disabled version: %v", err)
	}
	if _, err := disks.CreateSnapshot(ctx, &computepb.CreateSnapshotDiskRequest{Project: p, Zone: za, Disk: zd,
		SnapshotResource: &computepb.Snapshot{Name: proto.String("snap-" + sfx)}}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("snapshot with a disabled version: %v", err)
	}
}

// TestComputeDiskLicensesAndSnapshotSchedulesThroughTheGoClient (#2206):
// disks.update of licenses (Ubuntu to Ubuntu Pro) and regionDisks.update
// of guestOsFeatures, a snapshot schedule resource policy added to and removed
// from a disk, and patched, through the official Go clients.
// covers: compute.disks.update, compute.regionDisks.update, compute.resourcePolicies.insert, compute.resourcePolicies.patch, compute.disks.addResourcePolicies, compute.disks.removeResourcePolicies
func TestComputeDiskLicensesAndSnapshotSchedulesThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	disks, err := computeapi.NewDisksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	rdisks, _ := computeapi.NewRegionDisksRESTClient(ctx, opts...)
	pols, _ := computeapi.NewResourcePoliciesRESTClient(ctx, opts...)
	p := h.Project()
	const region, za = "us-central1", "us-central1-a"
	sfx := fmt.Sprint(time.Now().UnixNano() % 1e6)
	ud, rd, pol := "lic-"+sfx, "rlic-"+sfx, "sched-"+sfx
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		if op, err := disks.Delete(c, &computepb.DeleteDiskRequest{Project: p, Zone: za, Disk: ud}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := rdisks.Delete(c, &computepb.DeleteRegionDiskRequest{Project: p, Region: region, Disk: rd}); err == nil {
			_ = op.Wait(c)
		}
		if op, err := pols.Delete(c, &computepb.DeleteResourcePolicyRequest{Project: p, Region: region, ResourcePolicy: pol}); err == nil {
			_ = op.Wait(c)
		}
	})
	const pro = "projects/ubuntu-os-pro-cloud/global/licenses/ubuntu-pro-2404-lts"
	wait(disks.Insert(ctx, &computepb.InsertDiskRequest{Project: p, Zone: za, DiskResource: &computepb.Disk{
		Name: proto.String(ud), SourceImage: proto.String("projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts")}}))
	wait(disks.Update(ctx, &computepb.UpdateDiskRequest{Project: p, Zone: za, Disk: ud, Paths: proto.String("licenses"),
		DiskResource: &computepb.Disk{Name: proto.String(ud), Licenses: []string{pro}}}))
	d, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: p, Zone: za, Disk: ud})
	if err != nil || len(d.GetLicenses()) != 1 || !strings.HasSuffix(d.GetLicenses()[0], pro) || len(d.GetLicenseCodes()) != 1 {
		t.Fatalf("after replacing Ubuntu with Ubuntu Pro: %v %v %v", d.GetLicenses(), d.GetLicenseCodes(), err)
	}
	if _, err := disks.Update(ctx, &computepb.UpdateDiskRequest{Project: p, Zone: za, Disk: ud, Paths: proto.String("licenses"),
		DiskResource: &computepb.Disk{Name: proto.String(ud), Licenses: []string{pro, "projects/debian-cloud/global/licenses/debian-12"}}}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("appending Debian's license: %v, want 400", err)
	}
	wait(rdisks.Insert(ctx, &computepb.InsertRegionDiskRequest{Project: p, Region: region, DiskResource: &computepb.Disk{
		Name: proto.String(rd), SizeGb: proto.Int64(10), ReplicaZones: []string{"zones/" + za, "zones/us-central1-b"}}}))
	wait(rdisks.Update(ctx, &computepb.UpdateRegionDiskRequest{Project: p, Region: region, Disk: rd, Paths: proto.String("guestOsFeatures"),
		DiskResource: &computepb.Disk{Name: proto.String(rd), GuestOsFeatures: []*computepb.GuestOsFeature{{Type: proto.String("GVNIC")}}}}))
	r, err := rdisks.Get(ctx, &computepb.GetRegionDiskRequest{Project: p, Region: region, Disk: rd})
	if err != nil || len(r.GetGuestOsFeatures()) != 1 || r.GetGuestOsFeatures()[0].GetType() != "GVNIC" {
		t.Errorf("the regional disk's guest OS features: %v %v", r.GetGuestOsFeatures(), err)
	}

	// userLicenses is not a field of the v1 Disk message the Go client
	// has (Google's beta API has it): set it over REST.
	body := `{"name":"` + rd + `","userLicenses":["projects/` + p + `/global/licenses/mine"]}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPatch, "http://"+h.Endpoint(EnvCompute)+"/compute/v1/projects/"+p+"/regions/"+region+"/disks/"+rd+"?paths=userLicenses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusOK {
		t.Errorf("PATCH ?paths=userLicenses: %v %v", res, err)
	} else {
		res.Body.Close()
	}
	// A snapshot schedule.
	if _, err := pols.Insert(ctx, &computepb.InsertResourcePolicyRequest{Project: p, Region: region, ResourcePolicyResource: &computepb.ResourcePolicy{
		Name: proto.String(pol), SnapshotSchedulePolicy: &computepb.ResourcePolicySnapshotSchedulePolicy{
			Schedule: &computepb.ResourcePolicySnapshotSchedulePolicySchedule{HourlySchedule: &computepb.ResourcePolicyHourlyCycle{
				HoursInCycle: proto.Int32(4), StartTime: proto.String("04:13")}}}}}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("a start time off the hour: %v, want 400", err)
	}
	wait(pols.Insert(ctx, &computepb.InsertResourcePolicyRequest{Project: p, Region: region, ResourcePolicyResource: &computepb.ResourcePolicy{
		Name: proto.String(pol), SnapshotSchedulePolicy: &computepb.ResourcePolicySnapshotSchedulePolicy{
			Schedule: &computepb.ResourcePolicySnapshotSchedulePolicySchedule{DailySchedule: &computepb.ResourcePolicyDailyCycle{
				DaysInCycle: proto.Int32(1), StartTime: proto.String("04:00")}},
			RetentionPolicy: &computepb.ResourcePolicySnapshotSchedulePolicyRetentionPolicy{MaxRetentionDays: proto.Int32(14)}}}}))
	wait(disks.AddResourcePolicies(ctx, &computepb.AddResourcePoliciesDiskRequest{Project: p, Zone: za, Disk: ud,
		DisksAddResourcePoliciesRequestResource: &computepb.DisksAddResourcePoliciesRequest{ResourcePolicies: []string{"regions/" + region + "/resourcePolicies/" + pol}}}))
	if _, err := pols.Delete(ctx, &computepb.DeleteResourcePolicyRequest{Project: p, Region: region, ResourcePolicy: pol}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("deleting a schedule in use: %v, want 400", err)
	}
	wait(pols.Patch(ctx, &computepb.PatchResourcePolicyRequest{Project: p, Region: region, ResourcePolicy: pol,
		UpdateMask: proto.String("snapshotSchedulePolicy.retentionPolicy"),
		ResourcePolicyResource: &computepb.ResourcePolicy{SnapshotSchedulePolicy: &computepb.ResourcePolicySnapshotSchedulePolicy{
			RetentionPolicy: &computepb.ResourcePolicySnapshotSchedulePolicyRetentionPolicy{MaxRetentionDays: proto.Int32(7)}}}}))
	got, err := pols.Get(ctx, &computepb.GetResourcePolicyRequest{Project: p, Region: region, ResourcePolicy: pol})
	if err != nil || got.GetSnapshotSchedulePolicy().GetRetentionPolicy().GetMaxRetentionDays() != 7 ||
		got.GetSnapshotSchedulePolicy().GetSchedule().GetDailySchedule().GetStartTime() != "04:00" {
		t.Errorf("after the patch: %v %v", got, err)
	}
	wait(disks.RemoveResourcePolicies(ctx, &computepb.RemoveResourcePoliciesDiskRequest{Project: p, Zone: za, Disk: ud,
		DisksRemoveResourcePoliciesRequestResource: &computepb.DisksRemoveResourcePoliciesRequest{ResourcePolicies: []string{"regions/" + region + "/resourcePolicies/" + pol}}}))
	if d, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: p, Zone: za, Disk: ud}); err != nil || len(d.GetResourcePolicies()) != 0 {
		t.Errorf("after removeResourcePolicies: %v %v", d.GetResourcePolicies(), err)
	}
}

// TestGcloudComputeDiskKeysLicensesAndSchedules (#2206): gcloud compute
// disks create --kms-key, update-kms-key, update --append-licenses,
// resource-policies create snapshot-schedule and disks
// add-resource-policies, with egress to Google blocked and every command
// against Compute Engine here.
func TestGcloudComputeDiskKeysLicensesAndSchedules(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	if os.Getenv(EnvKMS) == "" {
		t.Skip("needs --services compute,kms")
	}
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	disks, err := computeapi.NewDisksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	sfx := fmt.Sprint(time.Now().UnixNano() % 1e6)
	const region, zone = "us-central1", "us-central1-a"
	keys, _ := diskKeys(t, h, "gdk-"+sfx, region)
	gdir, home := t.TempDir(), t.TempDir()
	name := gcloudSetup(t, cli, gdir)
	run := func(args ...string) (string, error) {
		args = append(args, "--project", h.Project(), "--quiet")
		cmd := exec.Command(gcloud, args...)
		cmd.Env = append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+name, "HOME="+home)
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	gc := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	dn, pol := "gk-"+sfx, "gs-"+sfx
	t.Cleanup(func() {
		_, _ = run("compute", "disks", "delete", dn, "--zone", zone)
		_, _ = run("compute", "resource-policies", "delete", pol, "--region", region)
	})
	gc("compute", "disks", "create", dn, "--zone", zone, "--image-family", "ubuntu-2404-lts", "--image-project", "ubuntu-os-cloud",
		"--kms-key", keys[region])
	if got := gc("compute", "disks", "describe", dn, "--zone", zone, "--format=value(diskEncryptionKey.kmsKeyName)"); !strings.Contains(got, keys[region]+"/cryptoKeyVersions/1") {
		t.Fatalf("disks describe: %s", got)
	}
	v2 := newPrimaryVersion(t, h, keys[region])
	gc("compute", "disks", "update-kms-key", dn, "--zone", zone)
	d, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: h.Project(), Zone: zone, Disk: dn})
	if err != nil || d.GetDiskEncryptionKey().GetKmsKeyName() != v2 {
		t.Errorf("after update-kms-key: %v %v, want %s", d.GetDiskEncryptionKey(), err, v2)
	}
	gc("compute", "disks", "update", dn, "--zone", zone, "--replace-license",
		"projects/ubuntu-os-cloud/global/licenses/ubuntu-2404-lts,projects/ubuntu-os-pro-cloud/global/licenses/ubuntu-pro-2404-lts")
	if got := gc("compute", "disks", "describe", dn, "--zone", zone, "--format=value(licenses)"); !strings.Contains(got, "ubuntu-pro-2404-lts") || strings.Contains(got, "ubuntu-os-cloud") {
		t.Errorf("licenses after --replace-license: %s", got)
	}
	if out, err := run("compute", "resource-policies", "create", "snapshot-schedule", pol, "--region", region,
		"--hourly-schedule", "4", "--start-time", "04:30", "--max-retention-days", "7"); err == nil {
		t.Errorf("a start time off the hour: %s", out)
	}
	gc("compute", "resource-policies", "create", "snapshot-schedule", pol, "--region", region,
		"--hourly-schedule", "4", "--start-time", "04:00", "--max-retention-days", "7", "--on-source-disk-delete", "apply-retention-policy")
	gc("compute", "disks", "add-resource-policies", dn, "--zone", zone, "--resource-policies", pol)
	if got := gc("compute", "disks", "describe", dn, "--zone", zone, "--format=value(resourcePolicies)"); !strings.Contains(got, pol) {
		t.Errorf("resource policies after add-resource-policies: %s", got)
	}
	gc("compute", "disks", "remove-resource-policies", dn, "--zone", zone, "--resource-policies", pol)
	gc("compute", "resource-policies", "delete", pol, "--region", region)
	gc("compute", "disks", "delete", dn, "--zone", zone)
}

// TestTerraformComputeDiskKeyAndSnapshotSchedule (#2206): a
// google_compute_disk with disk_encryption_key.kms_key_self_link, its key
// changed in place (the provider's disks.updateKmsKey), and a
// google_compute_resource_policy snapshot schedule attached by
// google_compute_disk_resource_policy_attachment, through `cloudburrow
// terraform` with egress to Google blocked.
func TestTerraformComputeDiskKeyAndSnapshotSchedule(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	if os.Getenv(EnvKMS) == "" {
		t.Skip("needs --services compute,kms")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	disks, err := computeapi.NewDisksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	m := newTFModule(t)
	const region, zone = "us-central1", "us-central1-a"
	sfx := fmt.Sprint(time.Now().UnixNano() % 1e6)
	keys, _ := diskKeys(t, h, "tdk-"+sfx, region, "global")
	name := "tfk-" + sfx
	t.Cleanup(func() {
		if op, err := disks.Delete(context.Background(), &computepb.DeleteDiskRequest{Project: m.project, Zone: zone, Disk: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	module := func(key string) string {
		return fmt.Sprintf(`
resource "google_compute_disk" "d" {
  project = %[1]q
  name    = %[2]q
  zone    = %[3]q
  size    = 10
  type    = "pd-balanced"
  disk_encryption_key {
    kms_key_self_link = %[4]q
  }
}

resource "google_compute_resource_policy" "s" {
  project = %[1]q
  name    = "%[2]s-sched"
  region  = %[5]q
  snapshot_schedule_policy {
    schedule {
      daily_schedule {
        days_in_cycle = 1
        start_time    = "04:00"
      }
    }
    retention_policy {
      max_retention_days    = 7
      on_source_disk_delete = "KEEP_AUTO_SNAPSHOTS"
    }
    snapshot_properties {
      labels = {
        purpose = "test"
      }
    }
  }
}

resource "google_compute_disk_resource_policy_attachment" "a" {
  project = %[1]q
  name    = google_compute_resource_policy.s.name
  disk    = google_compute_disk.d.name
  zone    = %[3]q
}
`, m.project, name, zone, key, region)
	}
	m.write(module(keys[region]))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	d, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: m.project, Zone: zone, Disk: name})
	if err != nil || !strings.HasPrefix(d.GetDiskEncryptionKey().GetKmsKeyName(), keys[region]+"/cryptoKeyVersions/") || len(d.GetResourcePolicies()) != 1 {
		t.Fatalf("the disk Terraform made: %v, %v", d, err)
	}
	m.planClean()
	id := d.GetId()
	m.write(module(keys["global"]))
	m.apply()
	if d, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: m.project, Zone: zone, Disk: name}); err != nil || d.GetId() != id ||
		!strings.HasPrefix(d.GetDiskEncryptionKey().GetKmsKeyName(), keys["global"]+"/cryptoKeyVersions/") {
		t.Fatalf("after the key change: %v, %v", d, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: m.project, Zone: zone, Disk: name}); computeCode(err) != http.StatusNotFound {
		t.Errorf("the disk after destroy: %v", err)
	}
}
