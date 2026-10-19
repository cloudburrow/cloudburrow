//go:build browser

package browser

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"testing"
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
// cosine similarity is higher with the text that describes it.
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
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}

	p := open(t)
	p.navigate("/ai/embeddings")
	p.waitFor(`document.querySelector("#emb-images") !== null`)
	p.setField("#emb-texts", "a red circle\na blue square")
	p.eval(fmt.Sprintf(`(() => {
		const bytes = Uint8Array.from(atob(%q), (c) => c.charCodeAt(0));
		const dt = new DataTransfer();
		dt.items.add(new File([bytes], "circle.png", { type: "image/png" }));
		const input = document.querySelector("#emb-images");
		input.files = dt.files;
		input.dispatchEvent(new Event("change"));
		return true;
	})()`, base64.StdEncoding.EncodeToString(b.Bytes())), new(bool))
	var picked []string
	p.eval(`[document.querySelector("#emb-model").value, document.querySelector("#emb-method").value]`, &picked)
	if len(picked) != 2 || picked[0] != model2 || picked[1] != "embedContent" {
		t.Fatalf("after choosing an image the model and method read %v; want %s and embedContent", picked, model2)
	}
	p.clickText("button", "Embed")
	p.waitFor(`document.querySelectorAll("#emb-results tbody tr").length === 3 || /\[error\]/.test(document.body.textContent)`)
	var rows [][]string
	p.eval(`[...document.querySelectorAll("#emb-results tbody tr")].map((tr) => [...tr.children].map((td) => td.textContent.trim()))`, &rows)
	if len(rows) != 3 {
		var text string
		p.eval(`document.querySelector("main").textContent`, &text)
		t.Fatalf("the Embeddings view shows %d results: %s", len(rows), text)
	}
	if r := rows[2]; r[0] != "[image] circle.png" || r[1] != "768" || r[2] != "260" {
		t.Errorf("the image's row reads %v; want [image] circle.png, 768 values and 260 tokens", r)
	}
	var sim []string
	p.eval(`[...document.querySelectorAll("#emb-similarity tbody td")].map((td) => td.textContent.trim())`, &sim)
	// Row 3 is the image: its similarity with "a red circle", then "a blue square".
	if len(sim) != 9 || !(sim[6] > sim[7]) {
		t.Errorf("the cosine similarity matrix reads %v; want the image nearer \"a red circle\"", sim)
	}
	var request string
	p.eval(`document.querySelector("#emb-request").textContent`, &request)
	if !strings.Contains(request, "inlineData") || !strings.Contains(request, model2+":embedContent") {
		t.Errorf("the request shown is %q; want the EmbedContent inlineData request", request)
	}
}
