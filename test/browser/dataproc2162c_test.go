//go:build browser

package browser

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestDataprocKernelGatewayThroughThePage (#2162), in the spark shard:
// Session templates' Create session template makes a Jupyter template
// with the Scala kernel (the session type and kernel chosen by keyboard
// and by click), read back through the console API; Create interactive
// session makes a Jupyter session from it, whose page gives its kernel
// (Scala) and the kernel gateway's URL, whose kernelspecs list the
// template with its language; Terminate and Delete end them. Every
// screen meets WCAG 2.1 AA.
func TestDataprocKernelGatewayThroughThePage(t *testing.T) {
	needService(t, "dataproc-sessions")
	if os.Getenv("CLOUDBURROW_TEST_SPARK") == "" {
		t.Skip("CLOUDBURROW_TEST_SPARK is not set: a session runs in the pinned apache/spark image")
	}
	project := uniqueProject(t)
	p := openWithin(t, 30*time.Minute)

	p.navigate("/dataproc/session-templates?project=" + project)
	p.clickText("#view button", "Create")
	p.waitFor(`["f-templateId", "f-region", "f-runtimeVersion", "f-idleTtl"].every((id) => document.getElementById(id) !== null)`)
	clickRadio(p, "sessionType", 1) // Jupyter
	clickRadio(p, "kernel", 1)      // Scala
	p.submitForm(map[string]string{"templateId": "scala-browser", "runtimeVersion": "2.2", "displayName": "Scala on Spark"}, "templateId")
	tmpl := "projects/" + project + "/locations/us-central1/sessionTemplates/scala-browser"
	p.waitFor(`[...document.querySelectorAll("#view tbody tr, #view h1")].some((r) => r.textContent.includes("scala-browser"))`)
	if got := dpDetail(t, "dataproc-templates", project, tmpl); !strings.Contains(got, "Jupyter") {
		t.Errorf("the template reads back as %s", got)
	}
	p.navigate("/dataproc/session-templates?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("scala-browser"))`)
	p.follow("scala-browser")
	var tmplPath string
	p.eval(`location.pathname + location.search`, &tmplPath)

	p.navigate("/dataproc/interactive?project=" + project)
	p.clickText("#view button", "Create")
	p.waitFor(`["f-sessionId", "f-template"].every((id) => document.getElementById(id) !== null)`)
	clickRadio(p, "sessionType", 1)
	clickRadio(p, "kernel", 1)
	p.submitForm(map[string]string{"sessionId": "scala-browser-1", "template": tmpl, "runtimeVersion": "2.2"}, "sessionId")
	session := "projects/" + project + "/locations/us-central1/sessions/scala-browser-1"
	got := dpWait(t, "dataproc-sessions", project, session, `"Active"`, 15*time.Minute)
	if !strings.Contains(got, "Scala (Scala on Spark)") && !strings.Contains(got, "Scala (") {
		t.Errorf("the session's kernel: %s", got)
	}
	m := regexp.MustCompile(`http://127\.0\.0\.1:\d+/kernels/[a-z0-9-]+-dot-us-central1/`).FindString(got)
	if m == "" {
		t.Fatalf("the session's page gives no kernel gateway: %s", got)
	}
	resp, err := http.Get(m + "api/kernelspecs")
	if err != nil {
		t.Fatal(err)
	}
	var specs struct {
		Kernelspecs map[string]struct {
			Spec struct {
				Language string `json:"language"`
			} `json:"spec"`
		} `json:"kernelspecs"`
	}
	err = json.NewDecoder(resp.Body).Decode(&specs)
	resp.Body.Close()
	if err != nil || specs.Kernelspecs["scala-browser"].Spec.Language != "scala" {
		t.Errorf("the kernel gateway's kernelspecs: %+v %v", specs, err)
	}
	p.navigate("/dataproc/interactive?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("scala-browser-1"))`)
	p.follow("scala-browser-1")
	p.waitFor(`document.querySelector("#view").textContent.includes("Kernel gateway")`)
	var sessPath string
	p.eval(`location.pathname + location.search`, &sessPath)

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.run(reducedMotion())
		k.navigate("/dataproc/session-templates/create?project=" + project)
		k.waitFor(`document.getElementById("f-templateId") !== null`)
		k.tabTo(`document.activeElement && document.activeElement.id.startsWith("f-sessionType-")`)
		k.run(chromedp.KeyEvent(kb.ArrowRight)) // Jupyter
		k.waitFor(`document.getElementById("f-kernel-1") !== null && !document.getElementById("f-kernel-1").closest("[hidden]")`)
		k.tabTo(`document.activeElement && document.activeElement.id.startsWith("f-kernel-")`)
		k.tabTo(`document.activeElement && document.activeElement.textContent.trim() === "Create" && document.activeElement.type === "submit"`)
		for _, fl := range k.keyboardFailures() {
			t.Errorf("create session template: %s", fl)
		}
	})

	auditScreens(t, []a11yPage{
		screen("create session template", "/dataproc/session-templates/create", project),
		{"session template", tmplPath, settled, ""},
		{"Jupyter session", sessPath, settled, ""},
	})

	p.navigate(sessPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("scala-browser-1")`)
	p.dpAct("Terminate")
	dpWait(t, "dataproc-sessions", project, session, `"Terminated"`, 3*time.Minute)
	p.navigate(tmplPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("scala-browser")`)
	p.dpAct("Delete")
}
