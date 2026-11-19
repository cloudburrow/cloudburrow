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
		`{"image":{"content":"bm90IGFuIGltYWdl"},"features":[{"type":"LOGO_DETECTION"}]},` +
		`{"image":{"content":"bm90IGFuIGltYWdl"},"features":[{"type":"LANDMARK_DETECTION"}]}]}`
	rs := annotate(t, req)
	if len(rs) != 4 {
		t.Fatalf("%d responses", len(rs))
	}
	full, _ := rs[0]["fullTextAnnotation"].(map[string]any)
	if text, _ := full["text"].(string); !strings.Contains(text, "INVOICE No. 2026-0417") {
		t.Errorf("text: %v", rs[0])
	}
	// The whole text with its bounds, then a word an entry (#1799).
	ta, _ := rs[0]["textAnnotations"].([]any)
	if len(ta) < 5 {
		t.Fatalf("textAnnotations: %v", rs[0]["textAnnotations"])
	}
	if a := ta[0].(map[string]any); a["boundingPoly"] == nil || a["description"] != full["text"] {
		t.Errorf("first textAnnotation: %v", a)
	}
	var words []string
	for _, a := range ta[1:] {
		a := a.(map[string]any)
		words = append(words, a["description"].(string))
		if vs, _ := a["boundingPoly"].(map[string]any)["vertices"].([]any); len(vs) != 4 {
			t.Errorf("word %v: vertices %v", a["description"], vs)
		}
	}
	if !strings.Contains(strings.Join(words, " "), "INVOICE No. 2026-0417") {
		t.Errorf("words: %v", words)
	}
	pages, _ := full["pages"].([]any)
	page, _ := pages[0].(map[string]any)
	blocks, _ := page["blocks"].([]any)
	if len(blocks) == 0 || page["confidence"] == nil {
		t.Fatalf("page: %v", page)
	}
	bl := blocks[0].(map[string]any)
	para := bl["paragraphs"].([]any)[0].(map[string]any)
	word := para["words"].([]any)[0].(map[string]any)
	syms := word["symbols"].([]any)
	last := syms[len(syms)-1].(map[string]any)
	if bl["blockType"] != "TEXT" || last["property"] == nil || syms[0].(map[string]any)["text"] == "" || word["boundingBox"] == nil {
		t.Errorf("first block: %v", bl)
	}
	if e, _ := rs[1]["error"].(map[string]any); e["message"] != "Bad image data." || e["code"] != 3.0 {
		t.Errorf("bad image: %v", rs[1])
	}
	if e, _ := rs[2]["error"].(map[string]any); e["code"] != 12.0 || !strings.Contains(e["message"].(string), "LOGO_DETECTION") {
		t.Errorf("logo: %v", rs[2])
	}
	// No local model answers "no landmark" as Vision does (docs/gaps.md):
	// LANDMARK_DETECTION is UNIMPLEMENTED before the image is read.
	if e, _ := rs[3]["error"].(map[string]any); e["code"] != 12.0 || !strings.Contains(e["message"].(string), "LANDMARK_DETECTION") {
		t.Errorf("landmark: %v", rs[3])
	}
}
