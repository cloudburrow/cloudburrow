package logagentimage

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/storageimage/storageimagetest"
)

func TestTagIsContentAddressed(t *testing.T) {
	a, b := Tag([]byte("one")), Tag([]byte("two"))
	if a != Tag([]byte("one")) || a == b || !strings.HasPrefix(a, Repository+":") {
		t.Errorf("Tag = %s, %s", a, b)
	}
	if !strings.HasPrefix(Dockerfile(), "FROM "+Base+"\n") || !strings.Contains(Dockerfile(), `ENTRYPOINT ["/cloudburrow-logging-agent"]`) {
		t.Errorf("Dockerfile =\n%s", Dockerfile())
	}
}

type recordingRunner struct {
	calls  []string
	exists bool
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if len(args) > 1 && args[0] == "image" && args[1] == "inspect" && !r.exists {
		return "", errors.New("no such image")
	}
	return "", nil
}

// Build builds once and reuses an image that exists; a CLI built without
// the binaries says how to get them; a file that is not the architecture's
// executable is refused.
func TestBuild(t *testing.T) {
	defer UseBinaries(storageimagetest.FS(nil))()
	if _, err := Build(context.Background(), &recordingRunner{}, "amd64"); !errors.Is(err, ErrNotEmbedded) || !strings.Contains(err.Error(), "make logging-agent-binaries") {
		t.Errorf("Build without an embedded binary = %v", err)
	}
	defer UseBinaries(storageimagetest.FS(map[string][]byte{
		"cloudburrow-logging-agent-linux-amd64": storageimagetest.ELF("amd64"),
		"cloudburrow-logging-agent-linux-arm64": storageimagetest.ELF("amd64"),
	}))()
	if _, err := Binary("arm64"); !errors.Is(err, ErrNotEmbedded) {
		t.Errorf("an amd64 build as arm64's = %v", err)
	}
	r := &recordingRunner{}
	tag, err := Build(context.Background(), r, "amd64")
	if err != nil || !strings.HasPrefix(tag, Repository) || len(r.calls) != 2 || !strings.Contains(r.calls[1], "docker build --platform linux/amd64 -t "+tag) {
		t.Errorf("Build = %s, %v; calls %v", tag, err, r.calls)
	}
	again := &recordingRunner{exists: true}
	if _, err := Build(context.Background(), again, "amd64"); err != nil || len(again.calls) != 1 {
		t.Errorf("a second Build rebuilt: %v %v", again.calls, err)
	}
}
