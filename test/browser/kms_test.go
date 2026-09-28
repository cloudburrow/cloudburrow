//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestKMSEditKeyLabelsThroughTheForm (#794), in the run shard, whose instance
// serves Cloud KMS: a key's page has Edit key, whose dialog is prefilled with
// the key's labels, the name shown disabled and the rotation schedule's two
// fields, empty for a key with none (#816). Cancel
// after a change closes it at once, with no question asked and nothing sent.
// A label key with a capital is sent and refused on the form with
// UpdateCryptoKey's own message; valid labels are saved with one PATCH, the
// dialog closes, and the console API's page for the key reads them.
func TestKMSEditKeyLabelsThroughTheForm(t *testing.T) {
	needService(t, "kms")
	p := open(t)
	project := uniqueProject(t)
	ring := "projects/" + project + "/locations/global/keyRings/browser-edit"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/kms?project="+project,
		`{"keyRingId":"browser-edit","location":"global"}`); code != http.StatusOK {
		t.Fatalf("create a key ring through the console API = %d: %s", code, body)
	}
	create, _ := json.Marshal(map[string]any{"Path": []string{ring}, "Action": "createkey",
		"Values": map[string]string{"cryptoKeyId": "k", "labels": `{"env":"dev"}`}})
	if code, body := consoleDo(t, http.MethodPost, "/api/actions/kms?project="+project, string(create)); code != http.StatusOK {
		t.Fatalf("create a key through the console API = %d: %s", code, body)
	}

	p.navigate("/kms/keyrings?project=" + project)
	p.clickText("#view tbody a", ring)
	p.clickText("#view tbody a", "k")
	openEdit := func() {
		p.waitFor(`document.querySelector(".modal") === null`)
		p.clickText("#view .page-actions button", "Edit key")
		p.waitFor(`document.querySelector(".modal.is-open #f-labels") !== null`)
	}
	setLabels := func(v string) {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #f-labels");
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, v), nil)
	}
	openEdit()

	var form struct {
		Labels, Name string
		NameDisabled bool
		Fields       []string
	}
	p.eval(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s);
		return { Labels: q("#f-labels").value, Name: q("#f-cryptoKeyId").value, NameDisabled: q("#f-cryptoKeyId").disabled,
		         Fields: [...document.querySelectorAll(".modal.is-open form input, .modal.is-open form textarea, .modal.is-open form select")].map((f) => f.id) }; })()`, &form)
	if form.Labels != "env=dev" || form.Name != "k" || !form.NameDisabled ||
		strings.Join(form.Fields, ",") != "f-cryptoKeyId,f-labels,f-rotationPeriod,f-nextRotationTime" {
		t.Errorf("the edit dialog is prefilled with %+v; want labels env=dev, the name k disabled, and the rotation schedule's fields", form)
	}

	// Cancel discards a change without asking (#783's rule).
	setLabels("env=staging")
	p.clickText(".modal.is-open .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.sent(http.MethodPatch, "/api/resources/kms"); len(sent) != 0 {
		t.Fatalf("Cancel sent the change: %v", sent)
	}

	openEdit()
	setLabels("Env=x")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector(".modal.is-open .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector(".modal.is-open .form-error").textContent`, &refusal)
	if want := `InvalidArgument: crypto_key.labels key "Env" is not a valid label key`; refusal != want {
		t.Errorf("a label key with a capital was refused on the form with %q, want %q", refusal, want)
	}
	// The refusal is on the page, not a browser error: it is forgiven.
	p.forgive()

	setLabels("env=prod\nteam=payments")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.sent(http.MethodPatch, "/api/resources/kms"); len(sent) != 2 {
		t.Errorf("the refused and the saved edit sent %d PATCHes, want 2: %v", len(sent), sent)
	}

	code, body := consoleDo(t, http.MethodGet, "/api/detail/kms?project="+project+"&name="+url.QueryEscape(ring)+"&name=k", "")
	var detail struct {
		Edit struct {
			Fields []struct{ Name, Default string }
		}
	}
	if err := json.Unmarshal([]byte(body), &detail); code != http.StatusOK || err != nil {
		t.Fatalf("read the key through the console API = %d (%v): %s", code, err, body)
	}
	saved := false
	for _, f := range detail.Edit.Fields {
		saved = saved || (f.Name == "labels" && f.Default == `{"env":"prod","team":"payments"}`)
	}
	if !saved {
		t.Errorf("the console API does not read the labels the browser saved: %s", body)
	}
}

// TestKMSEditKeyRotationThroughTheForm (#816), in the run shard: Edit key
// sets a key's rotation schedule. A period below the 24-hour minimum is sent
// and refused on the form with UpdateCryptoKey's own message; 30d with a next
// rotation time is saved, the dialog closes, and the console API's page for
// the key, and the reopened dialog, read the schedule back.
func TestKMSEditKeyRotationThroughTheForm(t *testing.T) {
	needService(t, "kms")
	p := open(t)
	project := uniqueProject(t)
	ring := "projects/" + project + "/locations/global/keyRings/browser-rotate"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/kms?project="+project,
		`{"keyRingId":"browser-rotate","location":"global"}`); code != http.StatusOK {
		t.Fatalf("create a key ring through the console API = %d: %s", code, body)
	}
	create, _ := json.Marshal(map[string]any{"Path": []string{ring}, "Action": "createkey",
		"Values": map[string]string{"cryptoKeyId": "k"}})
	if code, body := consoleDo(t, http.MethodPost, "/api/actions/kms?project="+project, string(create)); code != http.StatusOK {
		t.Fatalf("create a key through the console API = %d: %s", code, body)
	}
	next := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second).Format(time.RFC3339)

	p.navigate("/kms/keyrings?project=" + project)
	p.clickText("#view tbody a", ring)
	p.clickText("#view tbody a", "k")
	openEdit := func() {
		p.waitFor(`document.querySelector(".modal") === null`)
		p.clickText("#view .page-actions button", "Edit key")
		p.waitFor(`document.querySelector(".modal.is-open #f-rotationPeriod") !== null`)
	}
	set := func(id, v string) {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #%s");
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, id, v), nil)
	}
	field := func(id string) string {
		var v string
		p.eval(fmt.Sprintf(`document.querySelector(".modal.is-open #%s").value`, id), &v)
		return v
	}
	openEdit()
	if pr, nr := field("f-rotationPeriod"), field("f-nextRotationTime"); pr != "" || nr != "" {
		t.Errorf("a key with no schedule opens with rotation period %q, next rotation time %q", pr, nr)
	}

	set("f-rotationPeriod", "23h")
	set("f-nextRotationTime", next)
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector(".modal.is-open .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector(".modal.is-open .form-error").textContent`, &refusal)
	if want := "InvalidArgument: crypto_key.rotation_period 23h0m0s is out of range: it must be at least 24 hours and at most 876,000 hours"; refusal != want {
		t.Errorf("a rotation period of 23h was refused on the form with %q, want %q", refusal, want)
	}
	p.forgive()

	set("f-rotationPeriod", "30d")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.sent(http.MethodPatch, "/api/resources/kms"); len(sent) != 2 {
		t.Errorf("the refused and the saved edit sent %d PATCHes, want 2: %v", len(sent), sent)
	}

	code, body := consoleDo(t, http.MethodGet, "/api/detail/kms?project="+project+"&name="+url.QueryEscape(ring)+"&name=k", "")
	var detail struct {
		Edit struct {
			Fields []struct{ Name, Default string }
		}
	}
	if err := json.Unmarshal([]byte(body), &detail); code != http.StatusOK || err != nil {
		t.Fatalf("read the key through the console API = %d (%v): %s", code, err, body)
	}
	got := map[string]string{}
	for _, f := range detail.Edit.Fields {
		got[f.Name] = f.Default
	}
	if got["rotationPeriod"] != "30d" || got["nextRotationTime"] != next {
		t.Errorf("the console API reads rotation period %q, next rotation time %q; want 30d and %s", got["rotationPeriod"], got["nextRotationTime"], next)
	}
	openEdit()
	if pr, nr := field("f-rotationPeriod"), field("f-nextRotationTime"); pr != "30d" || nr != next {
		t.Errorf("the reopened dialog shows rotation period %q, next rotation time %q", pr, nr)
	}
}
