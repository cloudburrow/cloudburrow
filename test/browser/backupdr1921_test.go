//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	backupdr "google.golang.org/api/backupdr/v1"
	"google.golang.org/api/option"
)

// TestBackupDRMultiRegionLockAndJobsThroughThePages (#1921, #1924), in the
// sql shard. Create a backup vault's Multi-region, "Prevent deletion for
// duration specified in backup rule" and "Lock the enforced retention",
// drawn disabled before, make a vault in the us multi-region with
// MATCH_BACKUP_EXPIRE_TIME and its lock date; its page reads Multi-region
// and "Lock takes effect on". The Jobs page has Google's time range and its
// Current failures over the rows.
func TestBackupDRMultiRegionLockAndJobsThroughThePages(t *testing.T) {
	needService(t, "backupdr")
	bdr, err := backupdr.NewService(context.Background(), option.WithEndpoint("http://"+endpoint(t, envBackupDR)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	project := servedProject(t)
	vault := fmt.Sprintf("browser-mr-%d", time.Now().UnixNano()%1e6)
	name := "projects/" + project + "/locations/us/backupVaults/" + vault
	t.Cleanup(func() { _, _ = bdr.Projects.Locations.BackupVaults.Delete(name).Force(true).Do() })
	lock := time.Now().AddDate(1, 0, 0).UTC()

	p := open(t)
	p.navigate("/backupdr/create?project=" + project)
	p.waitFor(`document.querySelector("#f-id") !== null && document.querySelector('input[name="f-locationType-choice"][value="multi-region"]') !== null`)
	var disabled bool
	p.eval(`['input[name="f-locationType-choice"][value="multi-region"]', "#f-inherit", "#f-lock"].some((s) => document.querySelector(s).disabled)`, &disabled)
	if disabled {
		t.Fatal("Create a backup vault still draws Multi-region, retention inheritance or the lock disabled")
	}
	p.eval(`(() => { document.querySelector('input[name="f-locationType-choice"][value="multi-region"]').click();
		document.querySelector("#f-inherit").click(); document.querySelector("#f-lock").click(); return true; })()`, nil)
	p.waitFor(`document.querySelector("#f-multiRegion").getClientRects().length > 0 && document.querySelector("#f-lockDate").getClientRects().length > 0`)
	p.submitForm(map[string]string{"id": vault, "retentionDays": "1", "multiRegion": "us", "lockDate": lock.Format("2006-01-02T15:04")}, "id")
	p.waitFor(fmt.Sprintf(`location.pathname === "/backupdr" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, vault))
	v, err := bdr.Projects.Locations.BackupVaults.Get(name).Do()
	if err != nil || v.BackupRetentionInheritance != "MATCH_BACKUP_EXPIRE_TIME" || !strings.HasPrefix(v.EffectiveTime, lock.Format("2006-01-02T15:04")) {
		t.Fatalf("the vault made in the browser: %v %+v", err, v)
	}
	p.navigate("/backupdr/" + vault + "?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("Lock takes effect on")`)
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	if !strings.Contains(text, "Multi-region") {
		t.Errorf("the vault's page lacks its Multi-region location type: %q", text)
	}

	p.navigate("/backupdr-jobs?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("Current failures")`)
	p.eval(`document.querySelector("#view").textContent`, &text)
	if !strings.Contains(text, "1h, 6h, 12h, 24h") {
		t.Errorf("the Jobs page names no time range: %q", text)
	}
}
