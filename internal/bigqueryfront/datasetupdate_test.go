package bigqueryfront

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// oneDatasetEmulator keeps one dataset, d, as the patched emulator does
// (bigquery-emulator patch 0010): datasets.update saves the body it is sent.
type oneDatasetEmulator struct{ content string }

func (e *oneDatasetEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/datasets/d") {
		http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		_, _ = io.WriteString(w, e.content)
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		e.content = string(b)
		_, _ = w.Write(b)
	default:
		http.Error(w, `{"error":{"code":405}}`, http.StatusMethodNotAllowed)
	}
}

func sendDataset(t *testing.T, h http.Handler, method, body, ifMatch string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, base+"/datasets/d", strings.NewReader(body))
	if ifMatch != "" {
		r.Header.Set("If-Match", ifMatch)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	return w.Code, got
}

const datasetD = `{"kind":"bigquery#dataset","id":"p:d","datasetReference":{"projectId":"p","datasetId":"d"},"location":"US","creationTime":"1","description":"old","friendlyName":"Old","labels":{"a":"1","b":"2"}}`

func TestDatasetPatchMerges(t *testing.T) {
	e := &oneDatasetEmulator{content: datasetD}
	h := Wrap(e)
	code, got := sendDataset(t, h, http.MethodPatch, `{"description":"new","labels":{"a":"9","b":null,"c":"3"},"maxTimeTravelHours":"72","defaultTableExpirationMs":"7200000","defaultCollation":"und:ci","access":[{"role":"READER","specialGroup":"projectReaders"}]}`, "")
	if code != http.StatusOK {
		t.Fatalf("patch: %d %v", code, got)
	}
	_, read := do(t, h, http.MethodGet, base+"/datasets/d", "")
	labels, _ := read["labels"].(map[string]any)
	if read["description"] != "new" || read["friendlyName"] != "Old" || labels["a"] != "9" || labels["c"] != "3" || labels["b"] != nil ||
		read["maxTimeTravelHours"] != "72" || read["defaultTableExpirationMs"] != "7200000" || read["defaultCollation"] != "und:ci" || read["access"] == nil {
		t.Errorf("after patch, get = %v", read)
	}
	if read["location"] != "US" || read["creationTime"] != "1" || read["etag"] == nil || read["etag"] != got["etag"] {
		t.Errorf("fixed fields or etag: get %v, patch answered %v", read, got)
	}
}

func TestDatasetUpdateReplaces(t *testing.T) {
	e := &oneDatasetEmulator{content: datasetD}
	h := Wrap(e)
	code, got := sendDataset(t, h, http.MethodPut, `{"friendlyName":"New","location":"US"}`, "")
	if code != http.StatusOK {
		t.Fatalf("update: %d %v", code, got)
	}
	_, read := do(t, h, http.MethodGet, base+"/datasets/d", "")
	if read["friendlyName"] != "New" || read["description"] != nil || read["labels"] != nil || read["id"] != "p:d" {
		t.Errorf("after update, get = %v", read)
	}
}

func TestDatasetEtagPrecondition(t *testing.T) {
	e := &oneDatasetEmulator{content: datasetD}
	h := Wrap(e)
	_, read := do(t, h, http.MethodGet, base+"/datasets/d", "")
	etag, _ := read["etag"].(string)
	if etag == "" {
		t.Fatalf("no etag: %v", read)
	}
	if code, got := sendDataset(t, h, http.MethodPatch, `{"description":"x"}`, "not-it"); code != http.StatusPreconditionFailed {
		t.Fatalf("stale etag: %d %v", code, got)
	}
	if !strings.Contains(e.content, `"old"`) {
		t.Fatalf("a refused patch changed the dataset: %s", e.content)
	}
	code, got := sendDataset(t, h, http.MethodPatch, `{"description":"x"}`, etag)
	if code != http.StatusOK || got["etag"] == etag {
		t.Fatalf("matching etag: %d %v (etag was %s)", code, got, etag)
	}
	if code, _ := sendDataset(t, h, http.MethodPatch, `{"description":"y"}`, etag); code != http.StatusPreconditionFailed {
		t.Errorf("the old etag after a change: %d, want 412", code)
	}
}

func TestDatasetUpdateRefusals(t *testing.T) {
	for _, body := range []string{
		`{"maxTimeTravelHours":"36"}`,
		`{"maxTimeTravelHours":"200"}`,
		`{"defaultTableExpirationMs":"1000"}`,
		`{"location":"EU"}`,
	} {
		e := &oneDatasetEmulator{content: datasetD}
		if code, got := sendDataset(t, Wrap(e), http.MethodPatch, body, ""); code != http.StatusBadRequest {
			t.Errorf("%s: %d %v, want 400", body, code, got)
		}
		if e.content != datasetD {
			t.Errorf("%s changed the dataset", body)
		}
	}
	if code, _ := do(t, Wrap(&oneDatasetEmulator{content: datasetD}), http.MethodPatch, base+"/datasets/other", `{"description":"x"}`); code != http.StatusNotFound {
		t.Errorf("a missing dataset: %d, want 404", code)
	}
}

func TestGetServiceAccount(t *testing.T) {
	code, got := do(t, Wrap(&oneDatasetEmulator{}), http.MethodGet, base+"/serviceAccount", "")
	email, _ := got["email"].(string)
	if code != http.StatusOK || got["kind"] != "bigquery#getServiceAccountResponse" || email != serviceAccountEmail("p") ||
		!strings.HasPrefix(email, "bq-") || !strings.HasSuffix(email, "@bigquery-encryption.iam.gserviceaccount.com") {
		t.Errorf("getServiceAccount = %d %v", code, got)
	}
}
