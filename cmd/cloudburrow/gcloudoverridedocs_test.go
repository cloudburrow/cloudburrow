package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The override families gcloud-setup writes are stated in two docs (#590).
// Each says "Overrides written: `a`, `b` ...", and both must name exactly
// gcloudVerified: a doc naming a family that is not written sends a reader's
// gcloud to Google, and one that omits a written family hides it.
func TestDocumentedGcloudOverridesMatchGcloudVerified(t *testing.T) {
	var want []string
	for _, v := range gcloudVerified {
		want = append(want, v.property)
	}
	slices.Sort(want)
	sentence := regexp.MustCompile("Overrides written: ((?:`[a-z]+`(?:, | and |, and )?)+)")
	family := regexp.MustCompile("`([a-z]+)`")
	for _, doc := range []string{"../../docs/credentials.md", "../../docs/compatibility.md"} {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatal(err)
		}
		m := sentence.FindAllStringSubmatch(string(b), -1)
		if len(m) != 1 {
			t.Errorf("%s: %d \"Overrides written:\" sentences, want 1", doc, len(m))
			continue
		}
		var got []string
		for _, f := range family.FindAllStringSubmatch(m[0][1], -1) {
			got = append(got, f[1])
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s names %s; gcloud-setup writes %s", doc, strings.Join(got, ", "), strings.Join(want, ", "))
		}
	}
}
