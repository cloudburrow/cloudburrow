//go:build integration

package cluster_test

// `up` against an existing cluster, for real (#601): an older Knative is
// upgraded in place and its stamp updated, and a different node image is
// refused with a message naming `cloudburrow delete`. It is an external
// test package because it drives the components installer too, which
// imports this package.
//
// Like every integration test here it uses a unique cluster name and always
// deletes what it created, so a developer's own clusters are never touched.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/cluster"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
)

const itNodeImage = "kindest/node:v1.36.4"

func TestUpAgainstAnExistingCluster(t *testing.T) {
	for _, bin := range []string{"docker", "kind", "kubectl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	instance := fmt.Sprintf("it-%d", time.Now().UnixNano()%1e9)
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	newCluster := func(image string) *cluster.Cluster {
		c, err := cluster.New(cluster.Options{Name: "cloudburrow-" + instance, NodeImage: image,
			Kubeconfig: kubeconfig, CLIVersion: "v0.0.0-it"})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := newCluster(itNodeImage)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_ = c.Delete(ctx)
	})
	kubectl := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", kubeconfig}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("kubectl %v: %v (%s)", args, err, out)
		}
		return string(out)
	}
	annotate := func(key, value string) {
		t.Helper()
		kubectl("-n", cluster.StampNamespace, "annotate", "--overwrite", "configmap/"+cluster.StampName, key+"="+value)
	}
	stamp := func() cluster.Stamp {
		t.Helper()
		s, err := c.ReadStamp(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	var out strings.Builder
	if err := cluster.NewComponent(c, "", 5*time.Minute, &out).Start(ctx); err != nil {
		t.Fatalf("cluster start: %v\n%s", err, out.String())
	}
	if s := stamp(); s.NodeImage != itNodeImage || s.CLIVersion != "v0.0.0-it" {
		t.Fatalf("a created cluster is stamped %+v", s)
	}

	t.Run("an older Knative is upgraded in place", func(t *testing.T) {
		cfg, err := config.Load(config.Options{
			Args:   []string{"--name", instance, "--state-dir", t.TempDir(), "--services", "run"},
			Getenv: func(string) string { return "" },
		})
		if err != nil {
			t.Fatal(err)
		}
		var log strings.Builder
		comps := components.NewLifecycleComponent(kubeconfig, cfg, &log)
		if err := comps.Start(ctx); err != nil {
			t.Fatalf("first install: %v\n%s", err, log.String())
		}
		if s := stamp(); s.KnativeVersion != components.KnativeVersion {
			t.Fatalf("after an install the stamp is %+v, want Knative %s", s, components.KnativeVersion)
		}

		// A cluster an older release brought up.
		annotate(cluster.AnnotationKnativeVersion, "knative-v1.22.0")
		log.Reset()
		if err := comps.Start(ctx); err != nil {
			t.Fatalf("up against the older stamp: %v\n%s", err, log.String())
		}
		for _, want := range []string{
			"knative on this cluster is knative-v1.22.0; applying " + components.KnativeVersion + " in place",
			"applying serving-crds.yaml", "applying serving-core.yaml", "applying kourier.yaml",
		} {
			if !strings.Contains(log.String(), want) {
				t.Errorf("up did not report %q:\n%s", want, log.String())
			}
		}
		if s := stamp(); s.KnativeVersion != components.KnativeVersion {
			t.Errorf("the stamp is %+v after the upgrade, want Knative %s", s, components.KnativeVersion)
		}
		// Knative answers after the in-place apply, with the settings an
		// install makes.
		if got := kubectl("-n", "knative-serving", "get", "configmap", "config-deployment",
			"-o", "jsonpath={.data.progress-deadline}"); got != components.RevisionProgressDeadline {
			t.Errorf("progress-deadline = %q after the upgrade", got)
		}
	})

	t.Run("another node image is refused", func(t *testing.T) {
		annotate(cluster.AnnotationNodeImage, "kindest/node:v1.30.0")
		err := cluster.NewComponent(newCluster(itNodeImage), "", 5*time.Minute, &strings.Builder{}).Start(ctx)
		if !errors.Is(err, cluster.ErrNodeImageDrift) || !strings.Contains(err.Error(), "cloudburrow delete") {
			t.Fatalf("up against a v1.30.0 stamp = %v; want ErrNodeImageDrift naming `cloudburrow delete`", err)
		}
		// And a CLI pinning another image refuses the cluster this one made.
		annotate(cluster.AnnotationNodeImage, itNodeImage)
		err = cluster.NewComponent(newCluster("kindest/node:v1.35.0"), "", 5*time.Minute, &strings.Builder{}).Start(ctx)
		if !errors.Is(err, cluster.ErrNodeImageDrift) {
			t.Fatalf("a CLI pinning v1.35.0 = %v; want ErrNodeImageDrift", err)
		}
	})
}
