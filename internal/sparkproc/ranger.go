package sparkproc

// The RANGER optional component (#2162), as the component page documents
// it (https://docs.cloud.google.com/dataproc/docs/concepts/components/ranger,
// read 2026-11-23): the Ranger Admin server and web UI on the first
// master's port 6080, its database in "the MySql database instance running
// on the cluster's first master node", its audit logs in Solr (the SOLR
// component, which RANGER requires), and the admin user's password the one
// the cluster's dataproc:ranger.admin.password.uri holds, encrypted with
// dataproc:ranger.kms.key.uri (decrypted by the API before the cluster
// starts: ClusterSpec.RangerPassword). The Admin server's image is built
// on this machine from the pinned Apache Ranger 2.8.0 source release
// (ranger/Dockerfile); MySQL is the Docker official image Cloud SQL for
// MySQL 8.0 runs.

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ComponentRanger is the RANGER optional component.
const ComponentRanger = "RANGER"

//go:embed ranger/Dockerfile ranger/ranger.sh
var rangerFS embed.FS

var rangerFiles = []string{"Dockerfile", "ranger.sh"}

// RangerTag is the Ranger Admin image's tag: the build context's hash.
func RangerTag() string {
	h := sha256.New()
	for _, name := range rangerFiles {
		b, _ := rangerFS.ReadFile("ranger/" + name)
		h.Write([]byte(name))
		h.Write(b)
	}
	return "cloudburrow/dataproc-ranger:" + hex.EncodeToString(h.Sum(nil))[:16]
}

var rangerMu sync.Mutex

// RangerBuildTimeout bounds the image's build: a Maven build of Ranger's
// whole reactor.
const RangerBuildTimeout = 3 * time.Hour

// RangerImage builds the Ranger Admin image once, labelled for instance,
// and returns its tag.
func RangerImage(ctx context.Context, instance string) (string, error) {
	rangerMu.Lock()
	defer rangerMu.Unlock()
	tag := RangerTag()
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", tag).Run(); err == nil {
		return tag, nil
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range rangerFiles {
		b, _ := rangerFS.ReadFile("ranger/" + name)
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
	args := []string{"build", "-t", tag}
	if instance != "" {
		args = append(args, "--label", InstanceLabel+"="+instance)
	}
	ctx, cancel := context.WithTimeout(ctx, RangerBuildTimeout)
	defer cancel()
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

// startRanger starts the cluster's MySQL and Ranger Admin server on the
// master, Solr's ranger_audits core first (Solr runs: RANGER requires it).
func (c *Cluster) startRanger(ctx context.Context, e *Ecosystem) error {
	if e.Ranger == nil || e.RangerMySQL == "" {
		return &Unavailable{Msg: "the Spark runner serves no Ranger Admin image"}
	}
	if c.spec.RangerPassword == "" {
		return &Unavailable{Msg: "the Ranger admin password was not decrypted"}
	}
	image, err := e.Ranger(ctx)
	if err != nil {
		return &Unavailable{Msg: "build the Ranger Admin image from the Apache Ranger 2.8.0 source release: " + err.Error()}
	}
	dir := filepath.Join(c.dir, "ranger")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root := make([]byte, 16)
	if _, err := rand.Read(root); err != nil {
		return err
	}
	// The containers run as root (MySQL's entrypoint reads the file
	// before it drops to mysql), so the files need only be readable.
	for name, v := range map[string]string{"admin-password": c.spec.RangerPassword, "db-root-password": "Rr1" + hex.EncodeToString(root)} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o644); err != nil {
			return err
		}
	}
	if err := c.runComponent(ctx, "ranger-db", "", "-v", dir+":/cb/ranger:ro",
		"-e", "MYSQL_ROOT_PASSWORD_FILE=/cb/ranger/db-root-password", e.RangerMySQL); err != nil {
		return err
	}
	if err := c.waitLog(ctx, c.component("ranger-db"), regexp.MustCompile(`ready for connections.*port: 3306`), 5*time.Minute); err != nil {
		return err
	}
	// The audit core, from Solr's default config set.
	if out, err := c.d.docker(ctx, "exec", c.component("solr"), "solr", "create", "-c", "ranger_audits"); err != nil &&
		!strings.Contains(out, "already exists") {
		return &Unavailable{Msg: "create Solr's ranger_audits core: " + out}
	}
	if err := c.runComponent(ctx, "ranger", "", "-v", dir+":/cb/ranger:ro", image); err != nil {
		return err
	}
	return c.waitLog(ctx, c.component("ranger"), regexp.MustCompile(`CLOUDBURROW RANGER ADMIN READY`), 15*time.Minute)
}
