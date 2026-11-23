//go:build browser

package browser

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestDataprocRangerThroughThePage (#2162), on an instance serving
// Dataproc, Cloud Storage and Cloud KMS: the Ranger admin password is
// encrypted with a key of the instance's Cloud KMS and put in a bucket, as
// the Ranger component page documents; Create cluster, with Solr and
// Ranger checked and the two dataproc:ranger properties, makes a running
// cluster whose Configuration lists both components; its Web interfaces
// tab links Ranger through the component gateway, where Ranger's sign-in
// page takes the admin user and that password and shows the cluster's
// Ranger services (hive-dataproc among them); the tabs are reached by
// keyboard; every screen meets WCAG 2.1 AA.
func TestDataprocRangerThroughThePage(t *testing.T) {
	needService(t, "dataproc-clusters")
	if os.Getenv("CLOUDBURROW_TEST_SPARK") == "" {
		t.Skip("CLOUDBURROW_TEST_SPARK is not set: a cluster runs in the pinned apache/spark images")
	}
	kms := endpoint(t, envKMS)
	gcs := endpoint(t, envStorage)
	project := uniqueProject(t)
	const password = "Ranger2162b"

	// The key, the encrypted password and its bucket, through the REST APIs.
	loc := "http://" + kms + "/v1/projects/" + project + "/locations/global"
	ring := fmt.Sprintf("rgb%d", time.Now().UnixNano()%1e9)
	if code, body := do(t, http.MethodPost, loc+"/keyRings?keyRingId="+ring, "{}", ""); code != http.StatusOK {
		t.Fatalf("create the key ring = %d: %s", code, body)
	}
	key := "projects/" + project + "/locations/global/keyRings/" + ring + "/cryptoKeys/ranger"
	if code, body := do(t, http.MethodPost, loc+"/keyRings/"+ring+"/cryptoKeys?cryptoKeyId=ranger", `{"purpose":"ENCRYPT_DECRYPT"}`, ""); code != http.StatusOK {
		t.Fatalf("create the key = %d: %s", code, body)
	}
	code, body := do(t, http.MethodPost, "http://"+kms+"/v1/"+key+":encrypt",
		`{"plaintext":"`+base64.StdEncoding.EncodeToString([]byte(password+"\n"))+`"}`, "")
	if code != http.StatusOK {
		t.Fatalf("encrypt = %d: %s", code, body)
	}
	var enc struct {
		Ciphertext string `json:"ciphertext"`
	}
	if err := json.Unmarshal([]byte(body), &enc); err != nil {
		t.Fatal(err)
	}
	ct, err := base64.StdEncoding.DecodeString(enc.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	bucket := ring + "-ranger"
	if code, body := do(t, http.MethodPost, "http://"+gcs+"/storage/v1/b?project="+project, `{"name":"`+bucket+`"}`, ""); code != http.StatusOK {
		t.Fatalf("create the bucket = %d: %s", code, body)
	}
	if code, body := do(t, http.MethodPost, "http://"+gcs+"/upload/storage/v1/b/"+bucket+"/o?uploadType=media&name="+url.QueryEscape("admin-password.encrypted"),
		string(ct), ""); code != http.StatusOK {
		t.Fatalf("upload the encrypted password = %d: %s", code, body)
	}

	p := openWithin(t, 45*time.Minute)
	p.navigate("/dataproc/clusters?project=" + project)
	p.clickText("#view button", "Create")
	p.waitFor(`["f-clusterName", "f-comp_SOLR", "f-comp_RANGER", "f-properties"].every((id) => document.getElementById(id) !== null)`)
	p.submitForm(map[string]string{"clusterName": "ranger-b", "clusterType": "SINGLE", "comp_SOLR": "true", "comp_RANGER": "true",
		"properties": "dataproc:ranger.kms.key.uri=" + key + "\ndataproc:ranger.admin.password.uri=gs://" + bucket + "/admin-password.encrypted"}, "clusterName")
	cluster := "projects/" + project + "/regions/us-central1/clusters/ranger-b"
	t.Cleanup(func() {
		consoleDo(t, "POST", "/api/act/dataproc-clusters?project="+project+"&name="+cluster+"&action=delete", "{}")
	})
	dpWait(t, "dataproc-clusters", project, cluster, `"Running"`, 40*time.Minute)
	p.navigate("/dataproc/clusters?project=" + project)
	p.waitFor(listedRow("", "ranger-b"))
	p.follow("ranger-b")
	var clusterPath string
	p.eval(`location.pathname + location.search`, &clusterPath)
	p.clickText(`#view [role="tab"]`, "Configuration")
	p.waitFor(`["SOLR", "RANGER"].every((s) => document.querySelector("#view").textContent.includes(s))`)
	p.clickText(`#view [role="tab"]`, "Web interfaces")
	p.waitFor(`[...document.querySelectorAll("#view a")].some((a) => a.textContent.trim() === "Ranger" && a.href.includes("/component-gateway/"))`)
	var ranger string
	p.eval(`[...document.querySelectorAll("#view a")].find((a) => a.textContent.trim() === "Ranger").href`, &ranger)

	// Ranger's own sign-in page, through the gateway.
	r := open(t)
	r.navigate(ranger)
	r.waitFor(`document.querySelector("#username") !== null && document.querySelector("#password") !== null`)
	r.run(chromedp.SendKeys("#username", "admin"), chromedp.SendKeys("#password", password), chromedp.KeyEvent(kb.Enter))
	r.waitFor(`document.body.textContent.includes("hive-dataproc")`)

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.run(reducedMotion())
		k.navigate(clusterPath)
		k.waitFor(`document.querySelectorAll("#view [role=tab]").length > 1`)
		k.run(chromedp.Focus(chromedp.CSS(`#view [role=tab]`)))
		for i := 0; i < 8; i++ {
			var label string
			k.eval(`document.activeElement ? document.activeElement.textContent.trim() : ""`, &label)
			if label == "Web interfaces" {
				break
			}
			k.run(chromedp.KeyEvent(kb.ArrowRight))
		}
		k.waitFor(`document.activeElement && document.activeElement.textContent.trim() === "Web interfaces"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`[...document.querySelectorAll("#view a")].some((a) => a.textContent.trim() === "Ranger")`)
		for _, fl := range k.keyboardFailures() {
			t.Errorf("Ranger cluster: %s", fl)
		}
	})

	auditScreens(t, []a11yPage{
		screen("Dataproc clusters with a Ranger cluster", "/dataproc/clusters", project),
		{"Ranger cluster", clusterPath, settled, ""},
	})
	if !strings.Contains(clusterPath, "ranger-b") {
		t.Errorf("cluster page %q", clusterPath)
	}
}
