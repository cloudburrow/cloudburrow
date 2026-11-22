package sparkproc

// A procedure's container_image (#2104): "path of image in Artifact
// Registry", which "must only contain libraries to use in your procedure"
// (https://docs.cloud.google.com/bigquery/docs/spark-procedures, re-read
// 2026-11-21). Google's custom containers guide
// (https://docs.cloud.google.com/dataproc-serverless/docs/guides/custom-containers,
// re-read 2026-11-21) says what the runtime gives such an image: Spark's
// binaries mounted at /usr/lib/spark and its configuration at
// /etc/spark/conf, Java mounted from the host, the container run as the
// spark user (UID and GID 1099, a Dockerfile's USER ignored), the image's
// PYSPARK_PYTHON over the default, and jars put in /opt/spark/jars named
// by SPARK_EXTRA_CLASSPATH; and that it "cannot fetch containers from other
// registries". So the image is read from the instance's own Artifact
// Registry (never from Google's), loaded into the local Docker, and run
// with the runtime version's Spark and Java trees, copied once out of
// the pinned apache/spark image, mounted where the guide puts them.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// customJavaHome is where a custom container is given the runtime's Java.
const customJavaHome = "/usr/lib/jvm/managed-spark"

// customImage is a container_image loaded into the local Docker: its
// local reference, its environment, and the Spark and Java trees to
// mount into it.
type customImage struct {
	ref         string
	env         map[string]string
	spark, java string
}

// registryPath splits an Artifact Registry Docker path:
// {location}-docker.pkg.dev/{project}/{repository}/{image}[:tag|@digest].
func registryPath(uri string) (location, rest string, err error) {
	host, rest, ok := strings.Cut(uri, "/")
	loc, isAR := strings.CutSuffix(host, "-docker.pkg.dev")
	if !ok || !isAR || loc == "" || strings.Count(rest, "/") < 2 {
		return "", "", fmt.Errorf("container_image %s is not an Artifact Registry image ({location}-docker.pkg.dev/{project}/{repository}/{image}): "+
			"Managed Service for Apache Spark cannot fetch containers from other registries", uri)
	}
	return loc, rest, nil
}

// customImage reads uri from the instance's Artifact Registry, loads it
// into Docker, and finds runtimeImage's Spark and Java trees.
func (d *Docker) customImage(ctx context.Context, uri, runtimeImage string) (*customImage, error) {
	loc, rest, err := registryPath(uri)
	if err != nil {
		return nil, err
	}
	addr := ""
	if d.Registry != nil {
		addr = d.Registry()
	}
	if addr == "" {
		return nil, &Unavailable{Msg: "container_image is read from the instance's Artifact Registry, which this instance does not serve (enable Cloud Run)"}
	}
	// The registry answers {location}-docker.pkg.localhost on its port,
	// as a push to it names it; the dial goes to its address.
	_, port, _ := net.SplitHostPort(addr)
	ref, err := name.ParseReference(loc+"-docker.pkg.localhost:"+port+"/"+rest, name.Insecure)
	if err != nil {
		return nil, fmt.Errorf("container_image %s: %v", uri, err)
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithTransport(tr),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}))
	if err != nil {
		return nil, fmt.Errorf("container_image %s cannot be read from Artifact Registry: %v", uri, err)
	}
	digest, err := img.Digest()
	if err != nil {
		return nil, err
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("container_image %s: %v", uri, err)
	}
	env := map[string]string{}
	for _, kv := range cf.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	local := "cloudburrow.local/spark-container:" + digest.Hex[:32]
	if _, err := d.docker(ctx, "image", "inspect", local); err != nil {
		tag, err := name.NewTag(local)
		if err != nil {
			return nil, err
		}
		if err := d.load(ctx, tag, img); err != nil {
			return nil, &Unavailable{Msg: "load container_image " + uri + " into Docker: " + err.Error()}
		}
		d.logf("spark: loaded container_image %s as %s", uri, local)
	}
	sparkDir, javaDir, err := d.trees(ctx, runtimeImage)
	if err != nil {
		return nil, &Unavailable{Msg: err.Error()}
	}
	return &customImage{ref: local, env: env, spark: sparkDir, java: javaDir}, nil
}

// load writes img to `docker load` as tag.
func (d *Docker) load(ctx context.Context, tag name.Tag, img v1.Image) error {
	cmd := exec.CommandContext(ctx, "docker", "load")
	pr, pw := io.Pipe()
	cmd.Stdin = pr
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return err
	}
	werr := tarball.Write(tag, img, pw)
	_ = pw.CloseWithError(werr)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(out.String()))
	}
	return werr
}

// trees are runtimeImage's Spark (/opt/spark) and Java (JAVA_HOME,
// /opt/java/openjdk in the apache/spark images) copied to the cache once.
func (d *Docker) trees(ctx context.Context, runtimeImage string) (string, string, error) {
	d.treeMu.Lock()
	defer d.treeMu.Unlock()
	base := d.Cache
	if base == "" {
		base = filepath.Join(os.TempDir(), "cloudburrow-spark")
	}
	sum := sha256.Sum256([]byte(runtimeImage))
	dir := filepath.Join(base, "runtime-"+hex.EncodeToString(sum[:8]))
	sparkDir, javaDir := filepath.Join(dir, "spark"), filepath.Join(dir, "java")
	if _, err := os.Stat(filepath.Join(dir, "done")); err == nil {
		return sparkDir, javaDir, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	for _, p := range []string{sparkDir, javaDir} {
		if err := os.RemoveAll(p); err != nil {
			return "", "", err
		}
	}
	box := fmt.Sprintf("cb-spark-trees-%s-%d", hex.EncodeToString(sum[:4]), os.Getpid())
	if out, err := d.docker(ctx, "create", "--name", box, "--label", "cloudburrow.instance="+d.Instance, runtimeImage); err != nil {
		return "", "", errors.New("copy the runtime's Spark out of " + runtimeImage + ": " + out)
	}
	defer func() { _, _ = d.docker(context.Background(), "rm", "-f", "-v", box) }()
	for src, dst := range map[string]string{"/opt/spark": sparkDir, "/opt/java/openjdk": javaDir} {
		if out, err := d.docker(ctx, "cp", "-L", box+":"+src, dst); err != nil {
			return "", "", fmt.Errorf("copy %s out of %s: %s", src, runtimeImage, out)
		}
	}
	// Readable by the container's spark user.
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode()&os.ModeSymlink == 0 {
			_ = os.Chmod(p, info.Mode().Perm()|0o444|map[bool]os.FileMode{true: 0o111}[info.IsDir()])
		}
		return nil
	})
	return sparkDir, javaDir, os.WriteFile(filepath.Join(dir, "done"), nil, 0o644)
}
