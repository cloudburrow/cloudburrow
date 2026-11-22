package storageimage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// Base is dependencies.json's build.storageServerBase, the source of truth.
func TestBaseMatchesDependencies(t *testing.T) {
	raw, err := os.ReadFile("../../dependencies.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Components map[string]map[string]json.RawMessage `json:"components"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	var b struct{ Image, Digest string }
	if err := json.Unmarshal(d.Components["build"]["storageServerBase"], &b); err != nil {
		t.Fatal(err)
	}
	if want := b.Image + "@" + b.Digest; Base != want {
		t.Errorf("Base = %s; dependencies.json has %s", Base, want)
	}
}

// The tag is a content hash: the same binary and base give the same tag,
// and a different binary a different one.
func TestTagIsContentAddressed(t *testing.T) {
	a, b := Tag([]byte("one")), Tag([]byte("two"))
	if a != Tag([]byte("one")) || a == b || !strings.HasPrefix(a, Repository+":") {
		t.Errorf("Tag = %s, %s", a, b)
	}
	if !strings.HasPrefix(Dockerfile(), "FROM "+Base+"\n") || !strings.Contains(Dockerfile(), "USER 65532") {
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
// the binaries says how to get them.
func TestBuild(t *testing.T) {
	if _, err := Binary("amd64"); err != nil {
		if !errors.Is(err, ErrNotEmbedded) {
			t.Fatal(err)
		}
		if _, err := Build(context.Background(), &recordingRunner{}, "amd64", "w1"); !errors.Is(err, ErrNotEmbedded) {
			t.Errorf("Build without an embedded binary = %v", err)
		}
		t.Skip("no embedded binary in this build (make storage-binaries)")
	}
	r := &recordingRunner{}
	tag, err := Build(context.Background(), r, "amd64", "w1")
	if err != nil || !strings.HasPrefix(tag, Repository) || len(r.calls) != 2 || !strings.Contains(r.calls[1], "docker build --platform linux/amd64 --label io.cloudburrow.instance=w1 -t "+tag) {
		t.Errorf("Build = %s, %v; calls %v", tag, err, r.calls)
	}
	// Another instance, or another worktree with the same binary, reuses
	// the image the first built: the tag is the content's, not the
	// instance's (#2152).
	again := &recordingRunner{exists: true}
	if got, err := Build(context.Background(), again, "amd64", "w2"); err != nil || got != tag || len(again.calls) != 1 {
		t.Errorf("a second Build = %s, rebuilt: %v %v", got, again.calls, err)
	}
}
