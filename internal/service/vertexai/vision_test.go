package vertexai

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func annotate(t *testing.T, body string) []map[string]any {
	t.Helper()
	srv := httptest.NewServer(NewServer(nil).Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/images:annotate", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Responses []map[string]any `json:"responses"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&out) != nil {
		t.Fatalf("status %d", resp.StatusCode)
	}
	return out.Responses
}

func TestAnnotateImagesText(t *testing.T) {
	if testing.Short() {
		t.Skip("reads a page with Tesseract")
	}
	png, err := os.ReadFile("../../ocr/testdata/scan.png")
	if err != nil {
		t.Fatal(err)
	}
	req := `{"requests":[{"image":{"content":"` + base64.StdEncoding.EncodeToString(png) + `"},"features":[{"type":"DOCUMENT_TEXT_DETECTION"}]},` +
		`{"image":{"content":"bm90IGFuIGltYWdl"},"features":[{"type":"TEXT_DETECTION"}]},` +
		`{"image":{"content":"bm90IGFuIGltYWdl"},"features":[{"type":"LABEL_DETECTION"}]}]}`
	rs := annotate(t, req)
	if len(rs) != 3 {
		t.Fatalf("%d responses", len(rs))
	}
	full, _ := rs[0]["fullTextAnnotation"].(map[string]any)
	if text, _ := full["text"].(string); !strings.Contains(text, "INVOICE No. 2026-0417") {
		t.Errorf("text: %v", rs[0])
	}
	if ta, _ := rs[0]["textAnnotations"].([]any); len(ta) != 1 {
		t.Errorf("textAnnotations: %v", rs[0]["textAnnotations"])
	}
	if e, _ := rs[1]["error"].(map[string]any); e["message"] != "Bad image data." || e["code"] != 3.0 {
		t.Errorf("bad image: %v", rs[1])
	}
	if e, _ := rs[2]["error"].(map[string]any); e["code"] != 12.0 || !strings.Contains(e["message"].(string), "LABEL_DETECTION") {
		t.Errorf("label: %v", rs[2])
	}
}
