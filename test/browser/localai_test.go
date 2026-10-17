//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
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

// multimodalModel is the local CLIP model (#1454): a community ONNX
// conversion of OpenAI's CLIP, NOT a Google model.
const multimodalModel = "clip-vit-base-patch32-onnx-community"

// TestEmbeddingsScreenEmbedsAnImage (#1454), on the same -tags onnx instance
// as the test above: the CLIP model is downloaded (the pinned, checksummed
// fetch) and loaded through the Local models actions, and the Embeddings
// screen's image and text card embeds a red square chosen as a file with two
// captions, showing "a red square" closer to it than "a blue square".
func TestEmbeddingsScreenEmbedsAnImage(t *testing.T) {
	needService(t, "ai-local")
	needService(t, "ai-embed")
	state := func() (st string, loaded bool, bytes int64) {
		_, body := consoleDo(t, http.MethodGet, "/api/ai/local-models", "")
		var list struct {
			Models []struct {
				ID, State string
				Loaded    bool
				Bytes     int64
			}
		}
		_ = json.Unmarshal([]byte(body), &list)
		for _, m := range list.Models {
			if m.ID == multimodalModel {
				return m.State, m.Loaded, m.Bytes
			}
		}
		t.Fatalf("%s is not on the Local models page: %s", multimodalModel, body)
		return
	}
	st, loaded, size := state()
	if st != "downloaded" {
		if code, body := consoleDo(t, http.MethodPost, "/api/ai/local-models/"+multimodalModel+"/download",
			fmt.Sprintf(`{"confirmBytes":%d}`, size)); code >= 300 {
			t.Fatalf("download = %d %s", code, body)
		}
		deadline := time.Now().Add(15 * time.Minute)
		for st != "downloaded" && time.Now().Before(deadline) {
			time.Sleep(2 * time.Second)
			st, loaded, _ = state()
		}
	}
	if !loaded {
		if code, body := consoleDo(t, http.MethodPost, "/api/ai/local-models/"+multimodalModel+"/load", "{}"); code != http.StatusOK {
			t.Fatalf("load = %d %s", code, body)
		}
	}

	p := open(t)
	p.navigate("/ai/embeddings")
	p.waitFor(`document.querySelector("#emb-mm-file") !== null`)
	// A 64x64 red PNG, drawn on a canvas and set as the chosen file.
	var ok bool
	p.eval(`(() => {
	  const c = document.createElement("canvas"); c.width = 64; c.height = 64;
	  const g = c.getContext("2d"); g.fillStyle = "rgb(220,20,20)"; g.fillRect(0, 0, 64, 64);
	  const bin = atob(c.toDataURL("image/png").split(",")[1]);
	  const bytes = new Uint8Array(bin.length);
	  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
	  const dt = new DataTransfer();
	  dt.items.add(new File([bytes], "red.png", { type: "image/png" }));
	  document.querySelector("#emb-mm-file").files = dt.files;
	  return true;
	})()`, &ok)
	p.setField("#emb-mm-texts", "a red square\na blue square")
	p.clickText("button", "Embed image and captions")
	p.waitFor(`document.querySelectorAll("#emb-mm-results tbody tr").length === 2 || /\[error\]/.test(document.querySelector("#emb-mm-result").textContent)`)
	var cos []string
	p.eval(`[...document.querySelectorAll("#emb-mm-results td[data-cosine]")].map((td) => td.dataset.cosine)`, &cos)
	if len(cos) != 2 {
		var text string
		p.eval(`document.querySelector("#emb-mm-result").textContent`, &text)
		t.Fatalf("the image and text card shows %v: %s", cos, text)
	}
	var red, blue float64
	fmt.Sscan(cos[0], &red)
	fmt.Sscan(cos[1], &blue)
	if red <= blue {
		t.Errorf("cosine to \"a red square\" %v, to \"a blue square\" %v; want the matching caption closer", red, blue)
	}
}
