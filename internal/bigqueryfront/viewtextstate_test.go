package bigqueryfront

import "testing"

// TestViewTextsOutliveAFrontRestart (#1028, #1014): the client's text of a
// view's query, kept on the state directory, is restored by the next front,
// and the emulator's restart empties the file too.
func TestViewTextsOutliveAFrontRestart(t *testing.T) {
	path := viewTextsStateFile(t.TempDir())
	a := &viewTexts{}
	a.keep(path, t.Logf)
	want := viewText{engine: "SELECT `id#1` AS `id` FROM x", client: "SELECT id FROM p1.t WHERE id > 0"}
	key := viewKey("p", "d", "v")
	a.add(key, want)
	b := &viewTexts{}
	b.keep(path, t.Logf)
	if got, ok := b.get(key); !ok || got != want {
		t.Fatalf("restored %+v %v, want %+v", got, ok, want)
	}
	b.reset()
	c := &viewTexts{}
	c.keep(path, t.Logf)
	if _, ok := c.get(key); ok {
		t.Error("the emulator's restart left view texts in the file")
	}
}
