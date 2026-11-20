package backupdr

import (
	"strings"
	"testing"
	"time"
)

// A backup cannot be deleted inside its enforced retention, as Google's
// "You can delete a backup only after it exceeds its minimum enforced
// retention period" (bv-backup-manage, #1844); once it has passed, it can.
func TestBackupDeleteWaitsForEnforcedRetention(t *testing.T) {
	s, ds := setup(t)
	_, list := call(t, s, "GET", "/v1/"+ds+"/backups", "")
	bs, _ := list["backups"].([]any)
	if len(bs) != 1 {
		t.Fatalf("backups: %v", list)
	}
	name := bs[0].(map[string]any)["name"].(string)
	code, out := call(t, s, "DELETE", "/v1/"+name, "")
	if code != 400 || !strings.Contains(strings.ToLower(out["error"].(map[string]any)["message"].(string)), "enforced retention") {
		t.Fatalf("deleting inside the enforced retention: %d %v", code, out)
	}
	s.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	if code, out := call(t, s, "DELETE", "/v1/"+name, ""); code != 200 {
		t.Fatalf("deleting after the enforced retention: %d %v", code, out)
	}
}
