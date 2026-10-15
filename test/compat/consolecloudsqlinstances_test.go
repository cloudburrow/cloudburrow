//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestConsoleCloudSQLInstancesCreateRestartAndDelete (#1330): the Cloud
// SQL instances page creates an instance through the Admin API, which the
// official client then reads RUNNABLE once its server serves; Restart and
// Delete act on it, deletion protection refuses Delete with the API's
// words, and the instance `up` runs is refused.
func TestConsoleCloudSQLInstancesCreateRestartAndDelete(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	addr := consoleAddr(t, h)
	svc := sqlAdminService(t, h)
	project := h.Project()
	name := fmt.Sprintf("console-pg-%d", time.Now().UnixNano()%1e6)
	running := func() {
		t.Helper()
		for i := 0; i < 120; i++ {
			in, err := svc.Instances.Get(project, name).Do()
			if err == nil && in.State == "RUNNABLE" {
				return
			}
			time.Sleep(time.Second)
		}
		t.Fatalf("%s never became RUNNABLE", name)
	}
	del := func() (int, string) {
		return consoleDo(t, addr, http.MethodDelete, "/api/resources/cloudsql-instances?project="+project+"&name="+name, "")
	}
	code, out := consoleCreate(t, addr, "cloudsql-instances", project, map[string]string{
		"name": name, "databaseVersion": "POSTGRES_17", "tier": "db-custom-1-3840", "deletionProtection": "true"})
	if code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, out)
	}
	t.Cleanup(func() {
		if op, err := svc.Instances.Patch(project, name, sqlProtection(false)).Do(); err == nil {
			waitSQLOp(t, svc, project, op)
			_, _ = svc.Instances.Delete(project, name).Do()
		}
	})
	running()
	if code, out := del(); code == http.StatusOK || !strings.Contains(out, "protected") {
		t.Errorf("delete of a protected instance = %d: %s", code, out)
	}
	code, out = consoleDo(t, addr, http.MethodGet, "/api/resources/cloudsql-instances?project="+project, "")
	if code != http.StatusOK || !strings.Contains(out, name) || !strings.Contains(out, project+":us-central1:"+name) {
		t.Errorf("list = %d: %s", code, out)
	}

	code, out = consoleDo(t, addr, http.MethodPost, "/api/actions/cloudsql-instances?project="+project,
		fmt.Sprintf(`{"Name":%q,"Action":"restart"}`, name))
	if code != http.StatusOK {
		t.Fatalf("restart = %d: %s", code, out)
	}
	time.Sleep(2 * time.Second)
	running()

	// Lift the protection through the API, as gcloud would, then delete.
	op, err := svc.Instances.Patch(project, name, sqlProtection(false)).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	if code, out := del(); code != http.StatusOK {
		t.Fatalf("delete = %d: %s", code, out)
	}
	for i := 0; i < 120; i++ {
		if _, err := svc.Instances.Get(project, name).Do(); err != nil {
			wantHTTP(t, "get after the console's delete", err, http.StatusNotFound)
			break
		}
		time.Sleep(time.Second)
	}

	code, out = consoleDo(t, addr, http.MethodDelete, "/api/resources/cloudsql-instances?project="+project+"&name=postgres", "")
	if code == http.StatusOK || !strings.Contains(out, "cloudburrow up") {
		t.Errorf("delete of the instance up runs = %d: %s", code, out)
	}
}
