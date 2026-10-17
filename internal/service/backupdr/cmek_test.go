package backupdr

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKMS wraps by prefixing the key's name; disabled keys refuse.
type fakeKMS struct {
	mu       sync.Mutex
	disabled map[string]bool
}

func (k *fakeKMS) Encrypt(_ context.Context, key string, p []byte) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.disabled[key] {
		return nil, errors.New("the key is disabled")
	}
	return append([]byte(key+":"), p...), nil
}

func (k *fakeKMS) Decrypt(_ context.Context, key string, c []byte) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.disabled[key] || !strings.HasPrefix(string(c), key+":") {
		return nil, errors.New("the key is disabled")
	}
	return c[len(key)+1:], nil
}

// TestVaultEncryptionConfig (#1501): a vault created with
// encryptionConfig.kmsKeyName answers it; the key must be a CryptoKey in
// the vault's location and usable; without Cloud KMS it is FAILED_PRECONDITION. While the
// key is disabled, a backup into the vault and a restore from it fail
// FAILED_PRECONDITION; once it is enabled again they work.
func TestVaultEncryptionConfig(t *testing.T) {
	key := "projects/p/locations/us-central1/keyRings/r/cryptoKeys/k"
	body := func(k string) string {
		return `{"backupMinimumEnforcedRetentionDuration":"86400s","encryptionConfig":{"kmsKeyName":"` + k + `"}}`
	}
	s := NewServer("", &fakeSQL{runs: map[string]Run{}}, "")
	if code, _ := call(t, s, "POST", loc+"/backupVaults?backupVaultId=v", body(key)); code != 400 {
		t.Errorf("encryptionConfig without Cloud KMS: %d, want 400 FAILED_PRECONDITION", code)
	}
	kms := &fakeKMS{disabled: map[string]bool{}}
	s.UseKMS(kms)
	for _, k := range []string{"k", "projects/p/locations/europe-west1/keyRings/r/cryptoKeys/k"} {
		if code, _ := call(t, s, "POST", loc+"/backupVaults?backupVaultId=v", body(k)); code != 400 {
			t.Errorf("kmsKeyName %q: %d, want 400", k, code)
		}
	}
	kms.disabled[key] = true
	if code, _ := call(t, s, "POST", loc+"/backupVaults?backupVaultId=v", body(key)); code != 400 {
		t.Errorf("a disabled key: %d, want 400 FAILED_PRECONDITION", code)
	}
	kms.disabled[key] = false
	if code, out := call(t, s, "POST", loc+"/backupVaults?backupVaultId=v", body(key)); code != 200 {
		t.Fatalf("vault create: %d %v", code, out)
	}
	_, v := call(t, s, "GET", loc+"/backupVaults/v", "")
	if ec, _ := v["encryptionConfig"].(map[string]any); ec == nil || ec["kmsKeyName"] != key {
		t.Errorf("vault encryptionConfig: %v", v)
	}
	if code, out := call(t, s, "POST", loc+"/backupPlans?backupPlanId=plan", planBody); code != 200 {
		t.Fatalf("plan: %d %v", code, out)
	}
	if code, out := call(t, s, "POST", loc+"/backupPlanAssociations?backupPlanAssociationId=a",
		`{"backupPlan":"projects/p/locations/us-central1/backupPlans/plan","resource":"projects/p/instances/db","resourceType":"sqladmin.googleapis.com/Instance"}`); code != 200 {
		t.Fatalf("association: %d %v", code, out)
	}
	if code, out := call(t, s, "POST", loc+"/backupPlanAssociations/a:triggerBackup", `{"ruleId":"daily"}`); code != 200 {
		t.Fatalf("backup with a usable key: %d %v", code, out)
	}
	var backup string
	s.mu.Lock()
	for n := range s.st.Backups {
		backup = n
	}
	s.mu.Unlock()
	kms.disabled[key] = true
	if code, _ := call(t, s, "POST", loc+"/backupPlanAssociations/a:triggerBackup", `{"ruleId":"daily"}`); code != 400 {
		t.Errorf("backup with a disabled key: %d, want 400 FAILED_PRECONDITION", code)
	}
	if _, err := s.BackupRunID(backup); err == nil || !strings.Contains(err.Error(), "cannot be used") {
		t.Errorf("restore with a disabled key: %v", err)
	}
	kms.disabled[key] = false
	if _, err := s.BackupRunID(backup); err != nil {
		t.Errorf("restore with the key enabled again: %v", err)
	}
}

// listingSQL is a fakeSQL that lists its instance.
type listingSQL struct{ *fakeSQL }

func (listingSQL) Instances(string) []string { return []string{"db", "gone"} }

// TestResourceBackupConfigs (#1501): resourceBackupConfigs.list answers
// a config per Cloud SQL instance of the location, with each association
// protecting it.
func TestResourceBackupConfigs(t *testing.T) {
	s := NewServer("", listingSQL{&fakeSQL{runs: map[string]Run{}}}, "")
	if code, out := call(t, s, "GET", loc+"/resourceBackupConfigs", ""); code != 200 ||
		len(out["resourceBackupConfigs"].([]any)) != 1 {
		t.Fatalf("list before any association: %d %v", code, out)
	}
	call(t, s, "POST", loc+"/backupVaults?backupVaultId=v", `{"backupMinimumEnforcedRetentionDuration":"86400s"}`)
	call(t, s, "POST", loc+"/backupPlans?backupPlanId=plan", planBody)
	call(t, s, "POST", loc+"/backupPlanAssociations?backupPlanAssociationId=a",
		`{"backupPlan":"projects/p/locations/us-central1/backupPlans/plan","resource":"projects/p/instances/db","resourceType":"sqladmin.googleapis.com/Instance"}`)
	call(t, s, "POST", loc+"/backupPlanAssociations/a:triggerBackup", `{"ruleId":"daily"}`)
	var out map[string]any
	for i := 0; i < 100; i++ { // the backup is recorded once its run is done
		_, out = call(t, s, "GET", loc+"/resourceBackupConfigs", "")
		if strings.Contains(fmt.Sprint(out), "latestSuccessfulBackupTime") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c := out["resourceBackupConfigs"].([]any)[0].(map[string]any)
	if c["targetResourceType"] != "CLOUD_SQL_INSTANCE" || c["vaulted"] != true || c["targetResourceDisplayName"] != "db" {
		t.Errorf("config %v", c)
	}
	d := c["backupConfigsDetails"].([]any)[0].(map[string]any)
	if d["type"] != "BACKUPDR_BACKUP_PLAN" || d["backupVault"] != "projects/p/locations/us-central1/backupVaults/v" || d["latestSuccessfulBackupTime"] == nil {
		t.Errorf("details %v", d)
	}
	if code, _ := call(t, s, "GET", loc+"/resourceBackupConfigs?filter=x", ""); code != 400 {
		t.Errorf("a filter with no key=value: %d, want 400", code)
	}
	if code, out := call(t, s, "GET", "/v1/projects/p/locations/europe-west1/resourceBackupConfigs", ""); code != 200 ||
		len(out["resourceBackupConfigs"].([]any)) != 0 {
		t.Errorf("another location: %d %v", code, out)
	}
}

// TestResourceBackupConfigsFilter (#1536): fetch answers what list does,
// and both take gcloud's filter and orderBy and page.
func TestResourceBackupConfigsFilter(t *testing.T) {
	s := NewServer("", listingSQL{&fakeSQL{runs: map[string]Run{}}}, "")
	call(t, s, "POST", loc+"/backupVaults?backupVaultId=v", `{"backupMinimumEnforcedRetentionDuration":"86400s"}`)
	call(t, s, "POST", loc+"/backupPlans?backupPlanId=plan", planBody)
	call(t, s, "POST", loc+"/backupPlanAssociations?backupPlanAssociationId=a",
		`{"backupPlan":"projects/p/locations/us-central1/backupPlans/plan","resource":"projects/p/instances/db","resourceType":"sqladmin.googleapis.com/Instance"}`)
	names := func(path string) (int, []string, map[string]any) {
		code, out := call(t, s, "GET", path, "")
		var n []string
		cs, _ := out["resourceBackupConfigs"].([]any)
		for _, c := range cs {
			n = append(n, c.(map[string]any)["targetResourceDisplayName"].(string))
		}
		return code, n, out
	}
	q := func(f string) string { return url.QueryEscape(f) }
	for _, c := range []struct {
		filter string
		want   string
	}{
		{"", "[db]"},
		{`target_resource_display_name="db"`, "[db]"},
		{`target_resource_display_name="other"`, "[]"},
		{`target_resource_type="CLOUD_SQL_INSTANCE" AND vaulted=true`, "[db]"},
		{`vaulted=false`, "[]"},
		{`backup_configured=true AND backup_configs_details.type:"BACKUPDR_BACKUP_PLAN"`, "[db]"},
		{`backup_configs_details.backup_config_source_display_name:"a"`, "[db]"},
		{`backup_configs_details.type:"COMPUTE_ENGINE_RESOURCE_POLICY" OR vaulted=true`, "[db]"},
		{`NOT vaulted=true`, "[]"},
		{`target_resource_display_name!="db"`, "[]"},
	} {
		for _, verb := range []string{"", ":fetch"} {
			code, got, out := names(loc + "/resourceBackupConfigs" + verb + "?filter=" + q(c.filter))
			if code != 200 || fmt.Sprint(got) != c.want {
				t.Errorf("%s filter %q: %d %v %v, want %s", verb, c.filter, code, got, out, c.want)
			}
		}
	}
	for _, bad := range []string{"name=db", "vaulted=maybe", "vaulted=true AND", "(vaulted=true)"} {
		if code, _, _ := names(loc + "/resourceBackupConfigs:fetch?filter=" + q(bad)); code != 400 {
			t.Errorf("filter %q: %d, want 400", bad, code)
		}
	}
	if code, _, _ := names(loc + "/resourceBackupConfigs:fetch?orderBy=" + q("create_time")); code != 400 {
		t.Errorf("orderBy create_time: %d, want 400", code)
	}

	// Paging, over a project with three instances.
	s.sql = threeSQL{listingSQL{&fakeSQL{runs: map[string]Run{}}}}
	code, got, out := names(loc + "/resourceBackupConfigs:fetch?pageSize=2&orderBy=" + q("target_resource_display_name desc"))
	if code != 200 || fmt.Sprint(got) != "[c b]" || out["nextPageToken"] == nil {
		t.Fatalf("first page: %d %v %v", code, got, out)
	}
	code, got, out = names(loc + "/resourceBackupConfigs:fetch?pageSize=2&orderBy=" + q("target_resource_display_name desc") +
		"&pageToken=" + out["nextPageToken"].(string))
	if code != 200 || fmt.Sprint(got) != "[a]" || out["nextPageToken"] != nil {
		t.Errorf("second page: %d %v %v", code, got, out)
	}
	if code, _, _ := names(loc + "/resourceBackupConfigs:fetch?pageToken=bogus"); code != 400 {
		t.Errorf("a bogus page token: %d, want 400", code)
	}
}

// threeSQL serves instances a, b and c.
type threeSQL struct{ listingSQL }

func (threeSQL) Instances(string) []string { return []string{"a", "b", "c"} }

func (threeSQL) Instance(project, instance string) (Instance, bool) {
	return Instance{DatabaseVersion: "POSTGRES_17", Region: "us-central1"}, project == "p"
}

// TestTrial (#1536): ELIGIBLE, then SUBSCRIBED for 30 days, then ended
// or EXPIRED; once only.
func TestTrial(t *testing.T) {
	state := filepath.Join(t.TempDir(), "backupdr.json")
	s := NewServer("", &fakeSQL{runs: map[string]Run{}}, state)
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	us := "/v1/projects/p/locations/us"
	if code, out := call(t, s, "GET", us+"/trial", ""); code != 200 || out["state"] != "ELIGIBLE" || out["name"] != "projects/p/locations/us/trial" {
		t.Fatalf("getTrial: %d %v", code, out)
	}
	if code, _ := call(t, s, "GET", loc+"/trial", ""); code != 400 {
		t.Errorf("a regional trial: %d, want 400", code)
	}
	if code, _ := call(t, s, "POST", us+"/trial:end", `{"endReason":"MOVE_TO_PAID"}`); code != 400 {
		t.Errorf("ending an ELIGIBLE trial: %d, want 400", code)
	}
	code, out := call(t, s, "POST", us+"/trial:subscribe", `{}`)
	if code != 200 || out["state"] != "SUBSCRIBED" || out["endTime"] != "2026-10-31T00:00:00Z" {
		t.Fatalf("subscribe: %d %v", code, out)
	}
	if code, _ := call(t, s, "POST", us+"/trial:subscribe", `{}`); code != 400 {
		t.Errorf("subscribing twice: %d, want 400", code)
	}
	if code, _ := call(t, s, "POST", us+"/trial:end", `{}`); code != 400 {
		t.Errorf("end without a reason: %d, want 400", code)
	}
	// It survives a restart.
	s2 := NewServer("", &fakeSQL{runs: map[string]Run{}}, state)
	s2.now = func() time.Time { return now.Add(31 * 24 * time.Hour) }
	if code, out := call(t, s2, "GET", us+"/trial", ""); code != 200 || out["state"] != "EXPIRED" {
		t.Errorf("after 31 days: %d %v", code, out)
	}
	if code, out := call(t, s, "POST", us+"/trial:end", `{"endReason":"DISCONTINUED"}`); code != 200 ||
		out["state"] != "UNSUBSCRIBED" || out["endReason"] != "DISCONTINUED" {
		t.Errorf("end: %d %v", code, out)
	}
	if code, out := call(t, s, "POST", us+"/trial:subscribe", `{}`); code != 400 {
		t.Errorf("subscribing after the end: %d %v", code, out)
	}
	if code, out := call(t, s, "POST", loc+"/managementServers:msComplianceMetadata", `{"projectId":"p"}`); code != 200 || out["isAssuredWorkload"] != false {
		t.Errorf("msComplianceMetadata: %d %v", code, out)
	}
	if code, _ := call(t, s, "POST", loc+"/managementServers:msComplianceMetadata", `{}`); code != 400 {
		t.Errorf("msComplianceMetadata without projectId: %d, want 400", code)
	}
}
