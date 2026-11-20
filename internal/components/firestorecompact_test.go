package components

import (
	"path"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
)

// TestFirestoreCompactionFilesMatchTheFront (#1969): the emulator's
// container logs its collections, and takes restart requests, where the
// front looks for them.
func TestFirestoreCompactionFilesMatchTheFront(t *testing.T) {
	if path.Base(FirestoreHeapLog) != firestoreadmin.HeapLogFile || path.Dir(FirestoreHeapLog) != FirestoreDocumentsDir {
		t.Errorf("FirestoreHeapLog = %s; want %s in %s", FirestoreHeapLog, firestoreadmin.HeapLogFile, FirestoreDocumentsDir)
	}
	if FirestoreRestartFile != firestoreadmin.RestartFile {
		t.Errorf("FirestoreRestartFile = %q; want %q", FirestoreRestartFile, firestoreadmin.RestartFile)
	}
	script := strings.Join(firestoreBackend(false).Command, " ")
	for _, want := range []string{"-Xlog:gc,gc+init:file=" + FirestoreHeapLog, FirestoreDocumentsDir + "/" + FirestoreRestartFile + "$HOSTNAME"} {
		if !strings.Contains(script, want) {
			t.Errorf("the emulator's command does not name %q:\n%s", want, script)
		}
	}
}
