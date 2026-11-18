// Package tfconvertimage holds the build context of the TensorFlow SavedModel
// converter image (#1775): its Dockerfile and converter script, which the
// CLI builds on the host the first time a TENSORFLOW model is imported.
package tfconvertimage

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io"
)

// Context is the image's build context.
//
//go:embed Dockerfile convert.py
var Context embed.FS

// Tag is the image's tag: the build context's hash, so a changed
// Dockerfile or script is a new image, never a stale one.
func Tag() string {
	h := sha256.New()
	for _, name := range []string{"Dockerfile", "convert.py"} {
		b, _ := Context.ReadFile(name)
		h.Write([]byte(name))
		h.Write(b)
	}
	return "cloudburrow/tfconvert:" + hex.EncodeToString(h.Sum(nil))[:16]
}

// Tar is the build context as a tar, for `docker build -`.
func Tar() (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"Dockerfile", "convert.py"} {
		b, err := Context.ReadFile(name)
		if err != nil {
			return nil, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(b); err != nil {
			return nil, err
		}
	}
	return &buf, tw.Close()
}
