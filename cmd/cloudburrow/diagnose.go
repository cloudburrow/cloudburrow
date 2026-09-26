package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/doctor"
	"github.com/cloudburrow/cloudburrow/internal/version"
)

// `cloudburrow diagnose` (#293): one command that collects what a bug report
// needs into a bundle, and nothing a bug report must not contain.
//
// What it leaves out it never reads: the kubeconfig's contents, Kubernetes
// Secrets (so Cloud KMS key material, which lives in them), the ADC
// fixture's key file and Secret Manager payloads are not
// opened at all, so nothing depends on scrubbing them. What it does collect
// passes through the console's credential redaction, and pod specs have
// their env values removed, since an application may put a credential there
// literally. manifest.json lists every file, and every step that failed or
// was skipped, with the reason.

type diagnoseStep struct {
	File   string `json:"file,omitempty"`
	Step   string `json:"step"`
	Status string `json:"status"` // ok, failed, skipped
	Reason string `json:"reason,omitempty"`
}

type diagnoseManifest struct {
	Format    string         `json:"format"`
	Version   int            `json:"version"`
	Producer  string         `json:"producer"`
	Instance  string         `json:"instance"`
	Created   time.Time      `json:"created"`
	ClusterUp bool           `json:"cluster_up"`
	Running   bool           `json:"instance_running"`
	Steps     []diagnoseStep `json:"steps"`
	// Excluded names what is never collected, so a reader of the bundle
	// knows its absence is deliberate.
	Excluded []string `json:"excluded"`
}

type bundle struct {
	files map[string][]byte
	order []string
	m     diagnoseManifest
}

func (b *bundle) add(step, file string, data []byte) {
	b.files[file] = []byte(console.Redact(string(data)))
	b.order = append(b.order, file)
	b.m.Steps = append(b.m.Steps, diagnoseStep{File: file, Step: step, Status: "ok"})
}

func (b *bundle) fail(step string, err error) {
	b.m.Steps = append(b.m.Steps, diagnoseStep{Step: step, Status: "failed", Reason: console.Redact(err.Error())})
}

func (b *bundle) skip(step, reason string) {
	b.m.Steps = append(b.m.Steps, diagnoseStep{Step: step, Status: "skipped", Reason: reason})
}

func runDiagnose(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	out, _, rest, err := splitFlag(args, "o", false)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return errUsage
	}
	cfg, err := config.Load(config.Options{Args: rest, Output: stderr})
	if err != nil {
		return err
	}
	if out == "" {
		out = fmt.Sprintf("cloudburrow-diagnose-%s-%s.tar.gz", cfg.Name, time.Now().UTC().Format("20060102T150405Z"))
	}
	b := collectDiagnostics(ctx, cfg, newKubectl(cfg.KubeconfigPath()))
	if err := b.write(out); err != nil {
		return err
	}
	failed := 0
	for _, s := range b.m.Steps {
		if s.Status != "ok" {
			failed++
		}
	}
	fmt.Fprintf(stdout, "wrote %s: %d files, %d steps failed or skipped (listed in manifest.json)\n", out, len(b.order), failed)
	if !b.m.ClusterUp {
		fmt.Fprintf(stdout, "  the cluster is not running, so the bundle has configuration and doctor output only\n")
	}
	return nil
}

func collectDiagnostics(ctx context.Context, cfg config.Config, k *kubectl) *bundle {
	b := &bundle{files: map[string][]byte{}, m: diagnoseManifest{
		Format: "cloudburrow-diagnose", Version: 1, Producer: version.Get().String(),
		Instance: cfg.Name, Created: time.Now().UTC(),
		Excluded: []string{
			"kubeconfig contents", "Kubernetes Secrets", "the ADC fixture's private key",
			"Secret Manager payloads", "Cloud KMS key material", "environment variable values in pod specs",
		},
	}}
	b.add("version", "version.txt", []byte(version.Get().String()+"\n"))
	if c, err := json.MarshalIndent(cfg, "", "  "); err == nil {
		b.add("configuration", "config.json", c)
	} else {
		b.fail("configuration", err)
	}
	var doc bytes.Buffer
	doctor.Run(ctx, doctor.RealEnv(), doctorOptions(cfg)).Write(&doc)
	b.add("doctor", "doctor.txt", doc.Bytes())

	info, running := running(cfg)
	b.m.Running = running
	if running {
		if raw, err := json.MarshalIndent(info, "", "  "); err == nil {
			b.add("runtime file", "runtime.json", raw)
		}
		c := &http.Client{Timeout: 10 * time.Second}
		for _, g := range []struct{ step, file, path string }{
			{"readiness", "readyz.json", "/readyz"},
			{"recent admin events", "admin-events.json", "/admin/events?limit=1000"},
		} {
			req, err := http.NewRequest(http.MethodGet, "http://"+info.Control+g.path, nil)
			if err == nil && strings.HasPrefix(g.path, "/admin/") {
				// With the token, which is never itself in the bundle: the
				// bundle is what a user attaches to a bug report.
				req, err = adminRequest(cfg, http.MethodGet, "http://"+info.Control+g.path, nil)
			}
			var resp *http.Response
			if err == nil {
				resp, err = c.Do(req)
			}
			if err != nil {
				b.fail(g.step, err)
				continue
			}
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			_ = resp.Body.Close()
			b.add(g.step, g.file, raw)
			if g.file == "readyz.json" {
				b.add("port-forward supervisor state", "forwarders.json", forwarderState(raw))
			}
		}
		r, _ := buildStatusReport(cfg, liveStatus(cfg), "unknown", "")
		if raw, err := json.MarshalIndent(r, "", "  "); err == nil {
			b.add("status", "status.json", raw)
		}
	} else {
		for _, step := range []string{"runtime file", "readiness", "port-forward supervisor state", "recent admin events", "status"} {
			b.skip(step, "no `up` is running for this instance")
		}
	}

	if err := instanceRunning(ctx, cfg, k); err != nil {
		b.m.ClusterUp = false
		for _, step := range []string{"pods", "events", "knative services", "logs"} {
			b.skip(step, "the cluster is down: "+err.Error())
		}
	} else {
		b.m.ClusterUp = true
		for _, q := range []struct{ step, file, ns, kind string }{
			{"pods", "kubernetes/pods.json", cfg.Cluster.Namespace, "pods"},
			{"events", "kubernetes/events.json", cfg.Cluster.Namespace, "events"},
			{"knative services", "kubernetes/ksvc.json", "", "ksvc"},
		} {
			args := []string{"get", q.kind, "-o", "json"}
			if q.ns != "" {
				args = append([]string{"-n", q.ns}, args...)
			} else {
				args = append([]string{"-A", "-l", "cloudburrow.dev/instance=" + cfg.Name}, args...)
			}
			raw, err := k.run(ctx, k.args(args...))
			if err != nil {
				b.fail(q.step, err)
				continue
			}
			b.add(q.step, q.file, withoutEnvValues(raw))
		}
		var logs bytes.Buffer
		if err := streamLogs(ctx, cfg, logsOptions{tail: 200, format: "text"}, k, &logs); err != nil {
			b.fail("logs", err)
		} else {
			b.add("logs", "logs.txt", logs.Bytes())
		}
	}
	return b
}

// forwarderState picks the port-forwards' readiness out of /readyz, which is
// what the supervisor exposes outside the process.
func forwarderState(readyz []byte) []byte {
	var r readiness
	_ = json.Unmarshal(readyz, &r)
	out := map[string]bool{}
	for name, ok := range r.Components {
		if strings.HasPrefix(name, "forward:") {
			out[strings.TrimPrefix(name, "forward:")] = ok
		}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return b
}

// withoutEnvValues removes every value from pod env lists. A container's
// environment is where an application puts a credential when it has nowhere
// better; its names say what was configured, which is what a bug needs.
func withoutEnvValues(raw []byte) []byte {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return []byte("unparseable output withheld: it may contain environment values\n")
	}
	var walk func(v any, inEnv bool)
	walk = func(v any, inEnv bool) {
		switch t := v.(type) {
		case map[string]any:
			if inEnv {
				if _, ok := t["value"]; ok {
					t["value"] = "[REDACTED]"
				}
			}
			for k, c := range t {
				walk(c, k == "env")
			}
		case []any:
			for _, c := range t {
				walk(c, inEnv)
			}
		}
	}
	walk(doc, false)
	b, _ := json.MarshalIndent(doc, "", "  ")
	return b
}

func (b *bundle) write(path string) error {
	tmp, err := os.CreateTemp(dirOf(path), ".cloudburrow-diagnose-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_ = tmp.Chmod(0o600)
	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	add := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: b.m.Created}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	m, _ := json.MarshalIndent(b.m, "", "  ")
	if err := add("manifest.json", m); err != nil {
		return err
	}
	for _, name := range b.order {
		if err := add(name, b.files[name]); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func dirOf(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[:i+1]
	}
	return "."
}
