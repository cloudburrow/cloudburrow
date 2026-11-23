package sparkproc

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

//go:embed jupyter/Dockerfile jupyter/jupyter.sh jupyter/pyspark-startup.py jupyter/requirements.txt
var jupyterFS embed.FS

var jupyterFiles = []string{"Dockerfile", "jupyter.sh", "pyspark-startup.py", "requirements.txt"}

// JupyterTag is the Jupyter component's image tag for a Spark image: a
// hash of the image and the build context, so a changed context is a new
// image.
func JupyterTag(sparkImage string) string {
	h := sha256.New()
	h.Write([]byte(sparkImage))
	for _, name := range jupyterFiles {
		b, _ := jupyterFS.ReadFile("jupyter/" + name)
		h.Write([]byte(name))
		h.Write(b)
	}
	return "cloudburrow/dataproc-jupyter:" + hex.EncodeToString(h.Sum(nil))[:16]
}

var jupyterMu sync.Mutex

// JupyterImage builds the Jupyter component's image on sparkImage once,
// labelled for instance, and returns its tag.
func JupyterImage(ctx context.Context, sparkImage, instance string) (string, error) {
	jupyterMu.Lock()
	defer jupyterMu.Unlock()
	tag := JupyterTag(sparkImage)
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", tag).Run(); err == nil {
		return tag, nil
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range jupyterFiles {
		b, _ := jupyterFS.ReadFile("jupyter/" + name)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b))}); err != nil {
			return "", err
		}
		if _, err := tw.Write(b); err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	args := []string{"build", "--build-arg", "BASE=" + sparkImage, "-t", tag}
	if instance != "" {
		args = append(args, "--label", InstanceLabel+"="+instance)
	}
	cmd := exec.CommandContext(ctx, "docker", append(args, "-")...)
	cmd.Stdin = &buf
	out, err := cmd.CombinedOutput()
	if err != nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) > 15 {
			lines = lines[len(lines)-15:]
		}
		return "", fmt.Errorf("docker build: %v: %s", err, strings.Join(lines, "\n"))
	}
	return tag, nil
}
