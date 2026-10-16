//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
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
