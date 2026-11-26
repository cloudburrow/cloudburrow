package tfconvertimage

import (
	"archive/tar"
	"io"
	"regexp"
	"strings"
	"testing"
)

// The tag is the build context's hash, and Tar holds the whole context.
func TestTagAndTar(t *testing.T) {
	tag := Tag()
	if !regexp.MustCompile(`^cloudburrow/tfconvert:[0-9a-f]{16}$`).MatchString(tag) {
		t.Fatalf("Tag() = %q", tag)
	}
	t.Logf("tag %s", tag)
	r, err := Tar()
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(r)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if strings.Join(names, ",") != "Dockerfile,convert.py,transform.py" {
		t.Fatalf("tar holds %v", names)
	}
}
