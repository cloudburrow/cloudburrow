//go:build browser

package browser

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestCloudSQLExternalPrimaryThroughThePage (#1501), in the sql shard.
// Create instance with an external server host:port makes a source
// representation instance (ON_PREMISES_INSTANCE) instances.get reads; its
// page shows the external server, and its Create read replica makes a
// replica of it, whose Verify external sync settings answers what the
// unreachable server lacks (CONNECTION_FAILURE).
func TestCloudSQLExternalPrimaryThroughThePage(t *testing.T) {
	needService(t, "cloudsql-instances")
	svc := sqlAdminSDK(t)
	project := servedProject(t)
	n := time.Now().UnixNano() % 1e6
	src, rep := fmt.Sprintf("browser-ext-%d", n), fmt.Sprintf("browser-extr-%d", n)
	t.Cleanup(func() {
		for _, name := range []string{rep, src} {
			if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
				waitSQL(t, svc, project, d, 5*time.Minute)
			}
		}
	})

	p := open(t)
	p.navigate("/cloudsql-instances?project=" + project)
	p.clickText("#view button", "Create instance")
	p.submitForm(map[string]string{"name": src, "databaseVersion": "MYSQL_8_4", "sourceHostPort": "127.0.0.1:1",
		"sourceUser": "migrator", "sourcePassword": "Repl-pass1"}, "name")
	var got string
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(time.Second) {
		if in, err := svc.Instances.Get(project, src).Do(); err == nil {
			got = in.InstanceType + " " + in.OnPremisesConfiguration.HostPort
			break
		}
	}
	if got != "ON_PREMISES_INSTANCE 127.0.0.1:1" {
		t.Fatalf("instances.get after Create instance with an external server: %q", got)
	}

	page := "/cloudsql-instances/" + src + "?project=" + project
	p.navigate(page)
	p.waitFor(`document.querySelector("#view") !== null && document.querySelector("#view").textContent.includes("127.0.0.1:1")`)
	p.pageAction("Create read replica", map[string]string{"replica": rep}, "")
	state := ""
	for end := time.Now().Add(10 * time.Minute); time.Now().Before(end) && state != "RUNNABLE"; time.Sleep(2 * time.Second) {
		if in, err := svc.Instances.Get(project, rep).Do(); err == nil {
			state = in.State
			if in.MasterInstanceName != project+":"+src {
				t.Fatalf("the replica's primary is %q", in.MasterInstanceName)
			}
		}
	}
	if state != "RUNNABLE" {
		t.Fatalf("the replica is %q", state)
	}
	p.navigate("/cloudsql-instances/" + rep + "?project=" + project)
	rows := p.pageAction("Verify external sync settings", nil, "")
	if !strings.Contains(rows, "CONNECTION_FAILURE") {
		t.Errorf("Verify external sync settings answered %q; want CONNECTION_FAILURE", rows)
	}
}
