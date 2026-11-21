package console

import (
	"strings"
	"testing"
)

// TestAFileThatIsOneChoiceIsNotRequired (#1930): an action whose file
// field is shown only for one choice of another field, as Speech-to-Text's
// New transcription takes a Local upload or a Cloud storage file, is sent
// to /api/actions when the other choice is made; one whose file is always
// required goes to the upload route alone. A hidden file field sends no
// file.
func TestAFileThatIsOneChoiceIsNotRequired(t *testing.T) {
	always := Action{Fields: []Field{{Name: "file", Type: FileFieldType, Required: true}}}
	choice := Action{Fields: []Field{{Name: "source", Type: "radio"},
		{Name: "file", Type: FileFieldType, Required: true, VisibleWhen: map[string]string{"source": "Local upload"}}}}
	optional := Action{Fields: []Field{{Name: "file", Type: FileFieldType}}}
	if !requiresFile(always) || requiresFile(choice) || requiresFile(optional) {
		t.Errorf("requiresFile: always %v, choice %v, optional %v", requiresFile(always), requiresFile(choice), requiresFile(optional))
	}
	if !takesFile(choice) {
		t.Error("an action with a file choice takes no file")
	}
	js := consoleAsset(t, "console.js")
	if !strings.Contains(js, `e.field.type === "file" && !e.inactive && e.control.files && e.control.files[0]`) {
		t.Error("a hidden file field still sends its file")
	}
}
