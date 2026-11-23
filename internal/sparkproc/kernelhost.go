package sparkproc

// A Jupyter session's kernels (#2162): a Jupyter Server in a container of
// the image built from kernel/ on the session's runtime image, sharing
// the session's Spark container's network (an --internal network: it
// reaches the Spark Connect server on localhost:15002 and nothing else),
// its port 8888 published on the host's loopback by the session's relay.
// The Dataproc API's kernel gateway (internal/service/dataproc,
// gateway.go) starts and talks to kernels through it, as Google's kernel
// gateway (https://PROJECT_NUMBER-dot-REGION.kernels.googleusercontent.com,
// the GatewayClient URL of Google's dataproc-jupyter-plugin) does for a
// session's kernels.

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// KernelPort is the kernel server's port in the session's container.
const KernelPort = "8888"

// KernelRuntime is what a runtime's kernel image is built with, each a
// lock in kernel/: Scala, Almond's Scala version (the Spark's:
// almond-SCALA.lock); Spark, the Spark whose Scala Connect client the
// Scala kernel uses (connect-SPARK.lock); Python, the line of the PySpark
// kernel's package lock (requirements-PYTHON.txt: "3.5" or "4").
type KernelRuntime struct {
	Scala, Spark, Python string
}

//go:embed kernel/Dockerfile kernel/kernel.sh kernel/predef.sc kernel/pyspark-startup.py kernel/fetch.sh kernel/*.lock kernel/requirements-*.txt
var kernelFS embed.FS

// kernelFiles are the build context: every embedded file.
var kernelFiles = func() []string {
	entries, _ := kernelFS.ReadDir("kernel")
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}()

var kernelMu sync.Mutex

// KernelTag is the kernel image's tag for a Spark image and kernel
// runtime: a hash of them and the build context.
func KernelTag(sparkImage string, k KernelRuntime) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s", sparkImage, k.Scala, k.Spark, k.Python)
	for _, name := range kernelFiles {
		b, _ := kernelFS.ReadFile("kernel/" + name)
		h.Write([]byte(name))
		h.Write(b)
	}
	return "cloudburrow/dataproc-kernels:" + hex.EncodeToString(h.Sum(nil))[:16]
}

// KernelImage builds the kernel image on sparkImage once, labelled for
// instance, and returns its tag.
func KernelImage(ctx context.Context, sparkImage string, k KernelRuntime, instance string) (string, error) {
	kernelMu.Lock()
	defer kernelMu.Unlock()
	tag := KernelTag(sparkImage, k)
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", tag).Run(); err == nil {
		return tag, nil
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range kernelFiles {
		b, _ := kernelFS.ReadFile("kernel/" + name)
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
	args := []string{"build", "--build-arg", "BASE=" + sparkImage, "--build-arg", "SCALA=" + k.Scala,
		"--build-arg", "SPARK=" + k.Spark, "--build-arg", "PY=" + k.Python, "-t", tag}
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

var kernelReady = regexp.MustCompile(`Jupyter Server .* is running at|is running at:`)

// startKernels starts a Jupyter session's kernel server beside its
// Spark Connect server p.spark, and sets c.KernelAddr; a runtime with no
// kernel image pinned starts none.
func (d *Docker) startKernels(ctx context.Context, job Job, p *prepared, c *Connect) error {
	rt, _, ok := d.runtime(job)
	if !ok || rt.Kernel == nil {
		// The session still serves Spark Connect; the kernel gateway
		// answers that its runtime has no kernels (docs/gaps.md).
		d.logf("spark: the Jupyter session %s's runtime %s has no kernel image pinned", job.SparkJobID, job.RuntimeVersion)
		return nil
	}
	image, err := KernelImage(ctx, rt.Image, *rt.Kernel, d.Instance)
	if err != nil {
		return &Unavailable{Msg: "build the Jupyter session's kernel image: " + err.Error()}
	}
	name := p.spark + "-kernels"
	if out, err := d.docker(ctx, "run", "-d", "--name", name, "--label", d.instanceLabel(), "--network", "container:"+p.spark,
		"-e", "CB_CONNECT=sc://localhost:"+ConnectPort, image); err != nil {
		return &Unavailable{Msg: "start the Jupyter session's kernels: " + out}
	}
	p.cleanup = append(p.cleanup, func() { _, _ = d.docker(context.Background(), "rm", "-f", "-v", name) })
	deadline := time.Now().Add(3 * time.Minute)
	for {
		logs, _ := d.docker(ctx, "logs", name)
		if kernelReady.MatchString(logs) {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return &Unavailable{Msg: "the Jupyter session's kernel server did not start: " + lastLines(logs, 20)}
		}
		time.Sleep(time.Second)
	}
	out, err := d.docker(ctx, "port", p.relay, KernelPort)
	if err != nil {
		return &Unavailable{Msg: "find the kernel server's port: " + out}
	}
	c.KernelAddr = strings.TrimSpace(strings.Split(out, "\n")[0])
	d.logf("spark: the Jupyter session %s's kernels (%s) are on %s", job.SparkJobID, job.Kernel, c.KernelAddr)
	return nil
}
