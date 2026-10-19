//go:build browser

package browser

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/image/bmp"
)

// embeddingModel is the one embedding model served: a community ONNX
// conversion, NOT an official Google artifact.
const embeddingModel = "embeddinggemma-300m-onnx-community"

// TestStudioManagesLocalModelsAndTestsEmbeddings (#1396) needs an instance
// started from a -tags onnx build with -local-ai-embeddings. The Studio lists
// the served embedding model as downloaded and loaded and links to the
// Embeddings test view, where two texts sent with :embedContent show each
// vector's dimension, token count, truncated flag and first values, and their
// cosine similarity. The Local models page then unloads the model and loads
// it again with its buttons, and the Studio reports each state.
func TestStudioManagesLocalModelsAndTestsEmbeddings(t *testing.T) {
	needService(t, "ai-local")
	needService(t, "ai-embed")
	// Whatever happens below, leave the model loaded for the next test.
	t.Cleanup(func() {
		_, body := consoleDo(t, http.MethodGet, "/api/ai/local-models", "")
		var list struct {
			Models []struct {
				ID, State string
				Loaded    bool
			}
		}
		_ = json.Unmarshal([]byte(body), &list)
		for _, m := range list.Models {
			if m.ID == embeddingModel && m.State == "downloaded" && !m.Loaded {
				consoleDo(t, http.MethodPost, "/api/ai/local-models/"+embeddingModel+"/load", "{}")
			}
		}
	})
	p := open(t)
	row := fmt.Sprintf(`#pg-local-models tr[data-model=%q]`, embeddingModel)
	p.navigate("/ai/playground")
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row))
	var cells []string
	p.eval(fmt.Sprintf(`[...document.querySelectorAll(%q)].map((td) => td.textContent.trim())`, row+" td"), &cells)
	if len(cells) != 4 || cells[2] != "Downloaded" || cells[3] != "Loaded" {
		t.Fatalf("the Studio's row for %s reads %v; want Downloaded and Loaded", embeddingModel, cells)
	}

	p.clickText("#pg-local-models a", "Test embeddings")
	p.waitFor(`location.pathname === "/ai/embeddings" && document.querySelector("#emb-texts") !== null`)
	p.setField("#emb-texts", "A cat sat on the mat.\nA kitten rested on the rug.")
	p.setField("#emb-method", "embedContent")
	p.clickText("button", "Embed")
	p.waitFor(`document.querySelectorAll("#emb-results tbody tr").length === 2 || /\[error\]/.test(document.body.textContent)`)
	var rows [][]string
	p.eval(`[...document.querySelectorAll("#emb-results tbody tr")].map((tr) => [...tr.children].map((td) => td.textContent.trim()))`, &rows)
	if len(rows) != 2 {
		var text string
		p.eval(`document.querySelector("main").textContent`, &text)
		t.Fatalf("the Embeddings view shows %d results: %s", len(rows), text)
	}
	for _, r := range rows {
		// Text, Dimensions, Tokens, Truncated, L2 norm, First values.
		if len(r) != 6 || r[1] != "768" || r[2] == "" || r[2] == "0" || r[3] != "No" || strings.Count(r[5], ",") < 5 {
			t.Errorf("an embedding row reads %v; want 768 values, a token count, not truncated, and the first values", r)
		}
	}
	var sim []string
	p.eval(`[...document.querySelectorAll("#emb-similarity tbody td")].map((td) => td.textContent.trim())`, &sim)
	if len(sim) != 4 || sim[0] != "1.0000" || sim[1] != sim[2] || sim[1] == "1.0000" {
		t.Errorf("the cosine similarity matrix reads %v; want 1 on the diagonal and one symmetric value", sim)
	}

	p.navigate("/ai/local-models")
	model := fmt.Sprintf(`#local-models tr[data-model=%q]`, embeddingModel)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, model+` button[data-action="unload"]`))
	p.clickText(model+" button", "Unload")
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, model+` button[data-action="load"]`))
	p.navigate("/ai/playground")
	p.waitFor(fmt.Sprintf(`document.querySelector(%q)?.children[3]?.textContent === "Not loaded"`, row))

	p.navigate("/ai/local-models")
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, model+` button[data-action="load"]`))
	p.clickText(model+" button", "Load")
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, model+` button[data-action="unload"]`))
	p.navigate("/ai/playground")
	p.waitFor(fmt.Sprintf(`document.querySelector(%q)?.children[3]?.textContent === "Loaded"`, row))
}

// TestEmbeddingsPageEmbedsAnImage (#1454) needs EmbeddingGemma 2 downloaded
// and loaded (TestEmbedContentEmbedsImagesThroughThePredictionClient in
// test/compat does both). An image chosen on the Embeddings page switches
// the request to EmbedContent on that model and is embedded beside two
// texts; the image's row shows its 768 values and 260 tokens, and its
// cosine similarity is higher with the text that describes it. The picker
// accepts WebP and BMP (#1651): the same circle as a lossy WebP and as a
// BMP is embedded beside it, each nearer the text that describes it, the
// BMP (the same pixels) the PNG's vector.
func TestEmbeddingsPageEmbedsAnImage(t *testing.T) {
	needService(t, "ai-local")
	needService(t, "ai-embed")
	const model2 = "embeddinggemma-2-onnx-community"
	_, body := consoleDo(t, http.MethodGet, "/api/ai/local-models", "")
	var list struct {
		Models []struct {
			ID     string
			Loaded bool
		}
	}
	_ = json.Unmarshal([]byte(body), &list)
	loaded := false
	for _, m := range list.Models {
		loaded = loaded || (m.ID == model2 && m.Loaded)
	}
	if !loaded {
		t.Skipf("%s is not loaded; load it on the Local models page (TestEmbedContentEmbedsImagesThroughThePredictionClient does)", model2)
	}
	img := image.NewRGBA(image.Rect(0, 0, 768, 768))
	for y := 0; y < 768; y++ {
		for x := 0; x < 768; x++ {
			c := color.RGBA{255, 255, 255, 255}
			if (x-384)*(x-384)+(y-384)*(y-384) < 250*250 {
				c = color.RGBA{220, 20, 30, 255}
			}
			img.Set(x, y, c)
		}
	}
	var b, bm bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	if err := bmp.Encode(&bm, img); err != nil {
		t.Fatal(err)
	}
	// The same circle, made by cwebp -q 80 (test/compat/testdata).
	webp, err := os.ReadFile("../compat/testdata/circle-lossy.webp")
	if err != nil {
		t.Fatal(err)
	}

	p := open(t)
	p.navigate("/ai/embeddings")
	p.waitFor(`document.querySelector("#emb-images") !== null`)
	var accept string
	p.eval(`document.querySelector("#emb-images").accept`, &accept)
	for _, mt := range []string{"image/png", "image/jpeg", "image/webp", "image/bmp"} {
		if !strings.Contains(accept, mt) {
			t.Errorf("the image picker accepts %q; want %s among them", accept, mt)
		}
	}
	p.setField("#emb-texts", "a red circle\na blue square")
	p.eval(fmt.Sprintf(`(() => {
		const file = (b64, name, type) => new File([Uint8Array.from(atob(b64), (c) => c.charCodeAt(0))], name, { type });
		const dt = new DataTransfer();
		dt.items.add(file(%q, "circle.png", "image/png"));
		dt.items.add(file(%q, "circle.webp", "image/webp"));
		dt.items.add(file(%q, "circle.bmp", "image/bmp"));
		const input = document.querySelector("#emb-images");
		input.files = dt.files;
		input.dispatchEvent(new Event("change"));
		return true;
	})()`, base64.StdEncoding.EncodeToString(b.Bytes()), base64.StdEncoding.EncodeToString(webp),
		base64.StdEncoding.EncodeToString(bm.Bytes())), new(bool))
	var picked []string
	p.eval(`[document.querySelector("#emb-model").value, document.querySelector("#emb-method").value]`, &picked)
	if len(picked) != 2 || picked[0] != model2 || picked[1] != "embedContent" {
		t.Fatalf("after choosing an image the model and method read %v; want %s and embedContent", picked, model2)
	}
	p.clickText("button", "Embed")
	p.waitFor(`document.querySelectorAll("#emb-results tbody tr").length === 5 || /\[error\]/.test(document.body.textContent)`)
	var rows [][]string
	p.eval(`[...document.querySelectorAll("#emb-results tbody tr")].map((tr) => [...tr.children].map((td) => td.textContent.trim()))`, &rows)
	if len(rows) != 5 {
		var text string
		p.eval(`document.querySelector("main").textContent`, &text)
		t.Fatalf("the Embeddings view shows %d results: %s", len(rows), text)
	}
	for i, name := range []string{"circle.png", "circle.webp", "circle.bmp"} {
		if r := rows[2+i]; r[0] != "[image] "+name || r[1] != "768" || r[2] != "260" {
			t.Errorf("the image's row reads %v; want [image] %s, 768 values and 260 tokens", r, name)
		}
	}
	var sim []string
	p.eval(`[...document.querySelectorAll("#emb-similarity tbody td")].map((td) => td.textContent.trim())`, &sim)
	if len(sim) != 25 {
		t.Fatalf("the cosine similarity matrix reads %v; want 5 x 5", sim)
	}
	// Rows 3 to 5 are the images: each one's similarity with "a red
	// circle", then "a blue square".
	for r := 2; r < 5; r++ {
		if !(sim[r*5] > sim[r*5+1]) {
			t.Errorf("row %d of the cosine similarity matrix reads %v; want the image nearer \"a red circle\"", r+1, sim[r*5:r*5+5])
		}
	}
	if c, err := strconv.ParseFloat(sim[2*5+4], 64); err != nil || c < 0.999 {
		t.Errorf("the PNG and the BMP of the same pixels have cosine %q; want 1", sim[2*5+4])
	}
	var request string
	p.eval(`document.querySelector("#emb-request").textContent`, &request)
	if !strings.Contains(request, "inlineData") || !strings.Contains(request, model2+":embedContent") {
		t.Errorf("the request shown is %q; want the EmbedContent inlineData request", request)
	}
}

// jfkWAV is EmbeddingGemma 2's model card clip (a US government work),
// pinned by revision and SHA-256, as test/compat's
// TestEmbedContentEmbedsAudioThroughThePredictionClient fetches it.
const (
	jfkWAVURL    = "https://huggingface.co/datasets/Xenova/transformers.js-docs/resolve/542437c29cc5273cfd29e25c77696f2b349badd7/jfk.wav"
	jfkWAVSHA256 = "aa81c2552465568567e670f3823117e633900d16bd6202346a72f3c8464c74c8"
)

// TestEmbeddingsPageEmbedsAudioAndCloudStorage (#1651) needs EmbeddingGemma
// 2 downloaded and loaded with its audio encoder
// (TestEmbedContentEmbedsAudioThroughThePredictionClient in test/compat
// does both) and Cloud Storage. A WAV clip chosen on the Embeddings page,
// and the same clip uploaded to a bucket and named by its gs:// URI, are
// embedded beside two texts with EmbedContent: each row shows 768 values and
// 279 tokens, both are nearer the text that describes the speech, the two
// are the same vector, and the requests shown are an inlineData and a
// fileData part.
func TestEmbeddingsPageEmbedsAudioAndCloudStorage(t *testing.T) {
	needService(t, "ai-local")
	needService(t, "ai-embed")
	needService(t, "storage")
	const model2 = "embeddinggemma-2-onnx-community"
	_, body := consoleDo(t, http.MethodGet, "/api/ai/local-models", "")
	var list struct {
		Models []struct {
			ID     string
			Loaded bool
		}
	}
	_ = json.Unmarshal([]byte(body), &list)
	loaded := false
	for _, m := range list.Models {
		loaded = loaded || (m.ID == model2 && m.Loaded)
	}
	if !loaded {
		t.Skipf("%s is not loaded; load it on the Local models page (TestEmbedContentEmbedsAudioThroughThePredictionClient does)", model2)
	}
	resp, err := http.Get(jfkWAVURL)
	if err != nil {
		t.Skipf("fetching the test clip: %v", err)
	}
	clip, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if sum := sha256.Sum256(clip); err != nil || hex.EncodeToString(sum[:]) != jfkWAVSHA256 {
		t.Fatalf("the test clip: SHA-256 %x, %v", sum, err)
	}
	project := servedProject(t)
	bucket := fmt.Sprintf("browser-embed-%d", time.Now().UnixNano()%1e12)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage"+q, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("create a bucket through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/objects/storage?"+url.Values{"project": {project}, "name": {bucket, "jfk.wav"}}.Encode(), "")
		consoleDo(t, http.MethodDelete, "/api/resources/storage"+q+"&name="+bucket, "")
	})
	uploadObject(t, project, []string{bucket}, "jfk.wav", string(clip))

	p := open(t)
	p.navigate("/ai/embeddings")
	p.waitFor(`document.querySelector("#emb-audio") !== null && document.querySelector("#emb-gcs") !== null`)
	p.setField("#emb-texts", "a president's speech about serving your country\ncats sleeping on a couch")
	p.eval(fmt.Sprintf(`(() => {
		const bytes = Uint8Array.from(atob(%q), (c) => c.charCodeAt(0));
		const dt = new DataTransfer();
		dt.items.add(new File([bytes], "jfk.wav", { type: "audio/wav" }));
		const input = document.querySelector("#emb-audio");
		input.files = dt.files;
		input.dispatchEvent(new Event("change"));
		return true;
	})()`, base64.StdEncoding.EncodeToString(clip)), new(bool))
	var picked []string
	p.eval(`[document.querySelector("#emb-model").value, document.querySelector("#emb-method").value]`, &picked)
	if len(picked) != 2 || picked[0] != model2 || picked[1] != "embedContent" {
		t.Fatalf("after choosing a clip the model and method read %v; want %s and embedContent", picked, model2)
	}
	uri := "gs://" + bucket + "/jfk.wav"
	p.setField("#emb-gcs", uri)
	p.clickText("button", "Embed")
	p.waitFor(`document.querySelectorAll("#emb-results tbody tr").length === 4 || /\[error\]/.test(document.body.textContent)`)
	var rows [][]string
	p.eval(`[...document.querySelectorAll("#emb-results tbody tr")].map((tr) => [...tr.children].map((td) => td.textContent.trim()))`, &rows)
	if len(rows) != 4 {
		var text string
		p.eval(`document.querySelector("main").textContent`, &text)
		t.Fatalf("the Embeddings view shows %d results: %s", len(rows), text)
	}
	for i, want := range []string{"[audio] jfk.wav", "[file_data] " + uri} {
		if r := rows[2+i]; r[0] != want || r[1] != "768" || r[2] != "279" {
			t.Errorf("row %d reads %v; want %s, 768 values and 279 tokens", 3+i, r, want)
		}
	}
	var sim []string
	p.eval(`[...document.querySelectorAll("#emb-similarity tbody td")].map((td) => td.textContent.trim())`, &sim)
	// Rows 3 and 4 are the clip: nearer the speech text, and each other's twin.
	if len(sim) != 16 || !(sim[8] > sim[9]) || !(sim[12] > sim[13]) || sim[11] != "1.0000" {
		t.Errorf("the cosine similarity matrix reads %v; want the clip nearer the speech and both clips the same vector", sim)
	}
	var request string
	p.eval(`document.querySelector("#emb-request").textContent`, &request)
	if !strings.Contains(request, `"inlineData"`) || !strings.Contains(request, `"fileData"`) || !strings.Contains(request, uri) {
		t.Errorf("the requests shown are %q; want an inlineData and a fileData EmbedContent request", request)
	}
}
