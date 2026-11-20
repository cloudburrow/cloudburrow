package console

import (
	"encoding/json"
	"strings"
	"testing"
)

// trashProvider draws its rows' Delete as a trash icon (#1854).
type trashProvider struct{ mutableProvider }

func (trashProvider) RowDeleteIcon() bool { return true }

// A RowDeleteIconer's Delete reaches the client as deleteIcon; a provider
// that is not one keeps Delete in the row's menu.
func TestRowDeleteIconReachesTheClient(t *testing.T) {
	srv := serve(t,
		&trashProvider{mutableProvider{fakeProvider: fakeProvider{id: "trash", title: "Trash"}}},
		&mutableProvider{fakeProvider: fakeProvider{id: "plain", title: "Plain"}},
	)
	_, body := get(t, srv, "/api/services", nil)
	var got struct {
		Services []struct {
			ID         string `json:"id"`
			DeleteIcon bool   `json:"deleteIcon"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	seen := 0
	for _, s := range got.Services {
		switch s.ID {
		case "trash":
			seen++
			if !s.DeleteIcon {
				t.Error("trash: deleteIcon not sent")
			}
		case "plain":
			seen++
			if s.DeleteIcon {
				t.Error("plain: deleteIcon sent")
			}
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d of the 2 providers: %s", seen, body)
	}
}

// TestBarChartPinsAndCardFilterInTheClient (#1854): the client draws a
// Property's Chart as bars each labelled for a screen reader, a card's Pin
// as a toggle button kept per screen, the card filter as a labelled
// search box, an optional column off until chosen, and a trash icon named
// for its row.
func TestBarChartPinsAndCardFilterInTheClient(t *testing.T) {
	src := consoleAsset(t, "console.js")
	for _, want := range []string{
		`if (prop.chart && (prop.chart.bars || []).length) return el("dd", {}, barChart(prop));`,
		`"aria-label": b.label, title: b.label`,
		`"aria-pressed": on ? "true" : "false"`,
		"const pinKey = `${CARD_PINS_KEY}.${route.service}`;",
		`el("label", { for: "card-filter", class: "sr-only", text: data.cardFilter }), filterBox)`,
		`let off = new Set(Array.isArray(stored[service]) ? stored[service] : optional || []);`,
		"\"aria-label\": `${caps.deleteLabel || \"Delete\"} ${item.name}`",
		`...(caps.delete && !caps.deleteIcon ? [{`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("console.js is missing %q", want)
		}
	}
	// A Property with a chart stays comparable (tests compare them).
	a, b := Property{Label: "x"}, Property{Label: "x"}
	if a != b {
		t.Error("Property is not comparable")
	}
}
