package backupdr

import (
	"context"
	"errors"
	"fmt"
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
// protecting it; a filter is 501.
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
	if d["type"] != "BACKUPDR" || d["backupVault"] != "projects/p/locations/us-central1/backupVaults/v" || d["latestSuccessfulBackupTime"] == nil {
		t.Errorf("details %v", d)
	}
	if code, _ := call(t, s, "GET", loc+"/resourceBackupConfigs?filter=x", ""); code != 501 {
		t.Errorf("a filter: %d, want 501", code)
	}
	if code, out := call(t, s, "GET", "/v1/projects/p/locations/europe-west1/resourceBackupConfigs", ""); code != 200 ||
		len(out["resourceBackupConfigs"].([]any)) != 0 {
		t.Errorf("another location: %d %v", code, out)
	}
}
