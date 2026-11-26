//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// dmsBrowserBastion runs test/compat/testdata/dmsssh's SSH server on the
// kind network, from the source's image, and answers its address there.
func dmsBrowserBastion(t *testing.T) string {
	t.Helper()
	arch := runtime.GOARCH
	if out, err := exec.Command("docker", "version", "-f", "{{.Server.Arch}}").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		arch = strings.TrimSpace(string(out))
	}
	bin := filepath.Join(t.TempDir(), "dmsssh")
	build := exec.Command("go", "build", "-o", bin, "../compat/testdata/dmsssh")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build dmsssh: %v\n%s", err, out)
	}
	const image = "postgres:15-alpine@sha256:f7d23353e1b15400d22ebe31189f4d314b87a4c129cc400c8c2d8d4ca127bf81"
	name := fmt.Sprintf("cb-dms-bastion-%d", time.Now().UnixNano()%1e9)
	if out, err := exec.Command("docker", "create", "--name", name, "--network", "kind", "--entrypoint", "/dmsssh", image,
		"server", "-listen", ":22", "-user", "tunnel", "-password", "Ssh-pass1").CombinedOutput(); err != nil {
		t.Fatalf("docker create the bastion: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	if out, err := exec.Command("docker", "cp", bin, name+":/dmsssh").CombinedOutput(); err != nil {
		t.Fatalf("docker cp: %v: %s", err, out)
	}
	if out, err := exec.Command("docker", "start", name).CombinedOutput(); err != nil {
		t.Fatalf("docker start the bastion: %v: %s", err, out)
	}
	out, err := exec.Command("docker", "inspect", "-f", "{{(index .NetworkSettings.Networks \"kind\").IPAddress}}", name).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("the bastion's address: %v %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestDataMigrationExistingDestinationThroughThePage (#2257), in the sql
// shard: Create connection profile's source takes Google's forward-SSH
// tunnel (hostname, port, username, password) and its Encryption type;
// its Existing Cloud SQL instance makes a destination profile of an
// instance already made; Create migration job takes the databases to
// migrate; on the job's page Fetch source objects lists the source's
// databases under Objects, Demote destination (reached from the keyboard,
// confirmed by typing the instance's ID, as Google's dialog asks) makes the
// instance a replica, Start runs the job through the tunnel to its CDC
// phase, and Promote completes it, the instance standalone again; each
// screen passes the accessibility audit.
func TestDataMigrationExistingDestinationThroughThePage(t *testing.T) {
	needService(t, "datamigration")
	needService(t, "cloudsql")
	needService(t, "cloudsql-instances")
	svc := sqlAdminSDK(t)
	project := servedProject(t)
	n := time.Now().UnixNano() % 1e6
	srcID, dstID, jobID, inst := fmt.Sprintf("bx-src-%d", n), fmt.Sprintf("bx-dst-%d", n), fmt.Sprintf("bx-job-%d", n), fmt.Sprintf("bx-exist-%d", n)
	parent := "projects/" + project + "/locations/us-central1"
	ip, onSource := dmsBrowserSource(t)
	bastion := dmsBrowserBastion(t)

	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: inst, DatabaseVersion: "POSTGRES_15", Region: "us-central1",
		RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, inst).Do(); err == nil {
			waitSQL(t, svc, project, d, 5*time.Minute)
		}
	})
	waitSQL(t, svc, project, op, 10*time.Minute)
	t.Cleanup(func() {
		act := func(service, name, action string, values map[string]string) {
			b, _ := json.Marshal(map[string]any{"Path": []string{name}, "Action": action, "Values": values})
			consoleDo(t, http.MethodPost, "/api/actions/"+service+"?project="+url.QueryEscape(project), string(b))
		}
		act("datamigration", parent+"/migrationJobs/"+jobID, "delete", map[string]string{"force": "true"})
		act("datamigration-profiles", parent+"/connectionProfiles/"+dstID, "delete", map[string]string{})
		act("datamigration-profiles", parent+"/connectionProfiles/"+srcID, "delete", map[string]string{})
	})

	p := open(t)
	p.navigate("/dbmigrate/connection-profiles/create?project=" + project)
	p.submitForm(map[string]string{"kind": "postgresql", "profileId": srcID, "displayName": "Tunnelled source", "host": ip, "port": "5432",
		"username": "postgres", "password": "Src-pass1", "ssl": "NONE",
		"sshTunnel": "true", "sshHost": bastion, "sshPort": "22", "sshUser": "tunnel", "sshPassword": "Ssh-pass1"}, "profileId")
	p.waitForWithin(listedRow("/dbmigrate/connection-profiles", srcID), time.Minute)
	srcPath := "/dbmigrate/connection-profiles/" + url.PathEscape(parent+"/connectionProfiles/"+srcID) + "?project=" + url.QueryEscape(project)
	p.navigate(srcPath)
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, "tunnel@"+bastion+":22"))

	p.navigate("/dbmigrate/connection-profiles/create?project=" + project)
	p.submitForm(map[string]string{"kind": "cloudsql-existing", "profileId": dstID, "displayName": "Existing instance",
		"existingId": inst, "existingEngine": "postgresql"}, "profileId")
	p.waitForWithin(listedRow("/dbmigrate/connection-profiles", dstID), time.Minute)

	p.navigate("/dbmigrate/migrations/create?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#f-destination option")].some((o) => o.value === %q)`, parent+"/connectionProfiles/"+dstID))
	p.submitForm(map[string]string{"jobId": jobID, "displayName": "Into an existing instance", "source": parent + "/connectionProfiles/" + srcID,
		"destination": parent + "/connectionProfiles/" + dstID, "native": "true", "databases": "shop"}, "jobId")
	p.waitForWithin(listedRow("/dbmigrate/migrations", jobID), time.Minute)
	job := parent + "/migrationJobs/" + jobID
	jobPath := "/dbmigrate/migrations/" + url.PathEscape(job) + "?project=" + url.QueryEscape(project)

	// Fetch source objects: the job's Objects lists its databases.
	p.navigate(jobPath)
	p.clickText(p.findAction("Fetch source objects"), "Fetch source objects")
	for deadline := time.Now().Add(3 * time.Minute); ; {
		body := dmsDetail(t, project, job)
		if strings.Contains(body, `"Database":"shop"`) && strings.Contains(body, `"NOT_SELECTED"`) {
			break
		}
		if time.Now().After(deadline) {
			var alert string
			p.eval(`[...document.querySelectorAll("[role=alert], .toast, .error")].map((e) => e.textContent.trim()).join(" | ")`, &alert)
			t.Fatalf("the job's objects after Fetch source objects (the page says %q): %s", alert, body)
		}
		time.Sleep(3 * time.Second)
	}

	p.navigate(jobPath)
	p.waitFor(`document.querySelector("#view h1") !== null && [...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Demote destination")`)
	t.Run("keyboard", func(t *testing.T) {
		// Tab from the title reaches Demote destination; Enter opens its
		// dialog, whose typed confirmation is the instance's ID.
		p.eval(`document.querySelector("#view h1").setAttribute("tabindex", "-1"), document.querySelector("#view h1").focus(), true`, nil)
		reached := false
		for i := 0; i < 60 && !reached; i++ {
			p.run(chromedp.KeyEvent(kb.Tab))
			p.eval(`document.activeElement.matches("#view button") && document.activeElement.textContent.trim() === "Demote destination"`, &reached)
		}
		if !reached {
			t.Fatal("Tab never reaches Demote destination")
		}
		p.run(chromedp.KeyEvent(kb.Enter))
		if w := p.confirmWord(); w != inst {
			t.Errorf("Demote destination's confirmation word = %q, want the instance's ID %q", w, inst)
		}
		p.run(chromedp.KeyEvent(kb.Escape))
		p.waitFor(`document.querySelector(".modal") === null`)
	})

	t.Run("WCAG dialog", func(t *testing.T) {
		auditScreens(t, []a11yPage{{"Demote destination's dialog", jobPath, settled,
			`[...document.querySelectorAll("#view .page-actions button, #view .section-actions button")].find((b) => b.textContent.trim() === "Demote destination").click()`}})
	})
	p.navigate(jobPath)
	p.clickText(p.findAction("Demote destination"), "Demote destination")
	p.typeConfirmWord()
	for deadline := time.Now().Add(5 * time.Minute); ; {
		in, err := svc.Instances.Get(project, inst).Do()
		if err == nil && in.MasterInstanceName != "" && in.State == "RUNNABLE" && strings.Contains(dmsDetail(t, project, job), `"label":"Existing destination demoted","value":"Yes"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the instance was not demoted: %+v %v", in, err)
		}
		time.Sleep(3 * time.Second)
	}

	p.navigate(jobPath)
	p.clickText(p.findAction("Start"), "Start")
	for deadline := time.Now().Add(8 * time.Minute); ; {
		body := dmsDetail(t, project, job)
		if strings.Contains(body, "Running (CDC)") {
			break
		}
		if strings.Contains(body, `"Failed"`) || time.Now().After(deadline) {
			t.Fatalf("the job did not reach its CDC phase: %s", body)
		}
		time.Sleep(3 * time.Second)
	}
	onSource("INSERT INTO orders (item) VALUES ('after-start')")

	p.navigate(jobPath)
	p.clickText(p.findAction("Promote"), "Promote")
	p.pressConfirm("Promote")
	for deadline := time.Now().Add(5 * time.Minute); ; {
		if strings.Contains(dmsDetail(t, project, job), `"Completed"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the job did not complete after Promote")
		}
		time.Sleep(3 * time.Second)
	}
	in, err := svc.Instances.Get(project, inst).Do()
	if err != nil || in.MasterInstanceName != "" {
		t.Fatalf("the promoted instance: %+v %v", in, err)
	}

	t.Run("WCAG", func(t *testing.T) {
		auditScreens(t, []a11yPage{
			{"create a connection profile for an existing instance", "/dbmigrate/connection-profiles/create?project=" + url.QueryEscape(project), settled,
				`(() => { const k = document.querySelector("#f-kind"); k.value = "cloudsql-existing"; k.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`},
			{"create a connection profile with a forward-SSH tunnel and certificates", "/dbmigrate/connection-profiles/create?project=" + url.QueryEscape(project), settled,
				`(() => { const set = (id, v) => { const f = document.querySelector(id); if (f.type === "checkbox") f.checked = v; else f.value = v; f.dispatchEvent(new Event("change", { bubbles: true })); };
					set("#f-ssl", "SERVER_CLIENT"); set("#f-sshTunnel", true); return true; })()`},
			{"a source profile with a forward-SSH tunnel", srcPath, settled, ""},
			{"a migration job into an existing instance", jobPath, settled, ""},
		})
	})
}
