package console

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEditFormReviewIsSentAndDrawn (#1852): an edit form with Review opens
// a review of its changes before sending them, as Google's Cloud Run tabs'
// View diff & redeploy opens the diff, sent with Deploy changes.
func TestEditFormReviewIsSentAndDrawn(t *testing.T) {
	b, err := json.Marshal(EditForm{Label: "Containers", Submit: "View diff & redeploy", Review: "Deploy changes"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"review":"Deploy changes"`) {
		t.Errorf("the form is sent as %s; want its review label", b)
	}
	if b, _ := json.Marshal(EditForm{Label: "Edit"}); strings.Contains(string(b), "review") {
		t.Errorf("a form with no review is sent as %s", b)
	}
	src := consoleAsset(t, "console.js")
	for _, want := range []string{
		"function reviewChanges(spec, before, after)",
		"if (spec.review && !(await reviewChanges(spec, initial, fields.values()))) return;",
		`id: "review-diff"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("console.js lacks %q", want)
		}
	}
	// Both edit forms, the dialog and a tab's own, review first.
	if n := strings.Count(src, "await reviewChanges(spec, initial"); n != 2 {
		t.Errorf("%d edit forms review their changes; want the dialog's and the tab's", n)
	}
}

// TestFilterPropertiesListTheirValues (#1852): choosing a property in a
// list's filter lists the values its rows hold, as Google's "Filter
// services > Deployment type > Function".
func TestFilterPropertiesListTheirValues(t *testing.T) {
	src := consoleAsset(t, "console.js")
	for _, want := range []string{
		"function filterBar({ noun, columns, onChange, extra = [], valuesOf = null })",
		"const valuesMenu = () => {",
		"onChange: () => { page = 0; draw(); }, valuesOf });",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("console.js lacks %q", want)
		}
	}
}
