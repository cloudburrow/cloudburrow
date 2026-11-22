package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// gcDocker holds one unlabelled storage image from an old build and one of
// the user's own, and records what it is asked to run.
type gcDocker struct {
	calls   []string
	removed bool
}

func (d *gcDocker) Run(_ context.Context, _ string, args ...string) (string, error) {
	j := strings.Join(args, " ")
	d.calls = append(d.calls, j)
	switch {
	case j == "image ls -q --no-trunc":
		if d.removed {
			return "sha256:user\n", nil
		}
		return "sha256:old\nsha256:user\n", nil
	case strings.HasPrefix(j, "image inspect --format {{json .Id}}"):
		var out []string
		for _, id := range args[4:] {
			switch id {
			case "sha256:old":
				out = append(out, `"sha256:old"`+"\t"+`["dev.local/cloudburrow-storage:0123456789abcdef"]`+"\t{}\t113000000")
			case "sha256:user":
				out = append(out, `"sha256:user"`+"\t"+`["dev.local/myapp:1"]`+"\t"+`{"Labels":null}`+"\t5000")
			}
		}
		return strings.Join(out, "\n"), nil
	case strings.HasPrefix(j, "image inspect --format {{.Id}} "):
		return "", errors.New("no such image")
	case j == "image rm dev.local/cloudburrow-storage:0123456789abcdef":
		d.removed = true
		return "Untagged", nil
	case strings.HasPrefix(j, "ps"), strings.HasPrefix(j, "volume ls"):
		return "", nil
	}
	return "", errors.New("unexpected docker " + j)
}

// gc lists without --yes and removes nothing; with --yes it removes what it
// listed, and never the user's own image.
func TestRunGC(t *testing.T) {
	d := &gcDocker{}
	prev := dockerGCRunner
	dockerGCRunner = d
	t.Cleanup(func() { dockerGCRunner = prev })

	var out, errOut bytes.Buffer
	if err := runGC(context.Background(), nil, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "dev.local/cloudburrow-storage:0123456789abcdef") || strings.Contains(out.String(), "myapp") ||
		!strings.Contains(out.String(), "run `cloudburrow gc --yes`") || d.removed {
		t.Errorf("gc listed:\n%s", out.String())
	}
	out.Reset()
	if err := runGC(context.Background(), []string{"--yes"}, &out, &errOut); err != nil || !d.removed {
		t.Fatalf("gc --yes = %v; removed %v\n%s%s", err, d.removed, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "removed 1 image(s) (113.0MB) and 0 volume(s)") {
		t.Errorf("gc --yes printed:\n%s", out.String())
	}
	for _, c := range d.calls {
		if strings.Contains(c, "myapp") && strings.HasPrefix(c, "image rm") {
			t.Errorf("gc removed the user's image: %s", c)
		}
	}
	out.Reset()
	if err := runGC(context.Background(), nil, &out, &errOut); err != nil || strings.TrimSpace(out.String()) != "nothing to remove" {
		t.Errorf("gc after removal = %v:\n%s", err, out.String())
	}
	if err := runGC(context.Background(), []string{"extra"}, &out, &errOut); !errors.Is(err, errUsage) {
		t.Errorf("gc with an argument = %v", err)
	}
}
