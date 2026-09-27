//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// TestTwoInstancesRunSideBySideWithAPortBase is the two-environment recipe
// in docs/install.md, run (#584): two named instances, each with its own
// --port-base, started at the same time. Each must come up, and each must
// answer the official Cloud Storage client on its own port with its own
// buckets.
//
// It runs the built binary rather than runUp twice in this process, because
// two instances are two processes in real use.
func TestTwoInstancesRunSideBySideWithAPortBase(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "cloudburrow")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// Blocks well clear of the defaults and of each other, varied per run so
	// a leftover from an interrupted run does not hold them.
	first := 20000 + int(time.Now().UnixNano()%300)*100
	type instance struct {
		name, stateDir string
		base           int
	}
	suffix := time.Now().UnixNano() % 1e6
	instances := []instance{
		{fmt.Sprintf("pb-a-%d", suffix), t.TempDir(), first},
		{fmt.Sprintf("pb-b-%d", suffix), t.TempDir(), first + 100},
	}
	flags := func(in instance) []string {
		return []string{"--name", in.name, "--state-dir", in.stateDir,
			"--port-base", strconv.Itoa(in.base), "--services", "storage", "--mode", "ephemeral"}
	}
	for _, in := range instances {
		t.Cleanup(func() {
			// Always, even when the test failed part-way.
			_ = exec.Command(bin, append([]string{"delete"}, flags(in)...)...).Run()
			_ = exec.Command("kind", "delete", "cluster", "--name", "cloudburrow-"+in.name).Run()
		})
	}

	// Concurrently: the old recipe failed exactly when the second `up` ran
	// while the first held the default ports.
	var wg sync.WaitGroup
	errs := make([]error, len(instances))
	for i, in := range instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			args := append([]string{"up", "--detach", "--detach-timeout", "15m"}, flags(in)...)
			if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
				errs[i] = fmt.Errorf("up %s: %v\n%s", in.name, err, out)
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	clientFor := func(in instance) (*storage.Client, string) {
		t.Helper()
		// The Storage port is the default's distance from the base.
		port := in.base + (config.Default().Endpoints.Storage - config.DefaultPortBase)
		c, err := storage.NewClient(ctx,
			option.WithEndpoint(fmt.Sprintf("http://127.0.0.1:%d/storage/v1/", port)),
			option.WithoutAuthentication())
		if err != nil {
			t.Fatalf("storage client for %s: %v", in.name, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c, config.ProjectForName(in.name)
	}
	list := func(c *storage.Client, project string) map[string]bool {
		t.Helper()
		got := map[string]bool{}
		it := c.Buckets(ctx, project)
		for {
			b, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return got
			}
			if err != nil {
				t.Fatalf("list buckets in %s: %v", project, err)
			}
			got[b.Name] = true
		}
	}

	a, projectA := clientFor(instances[0])
	b, projectB := clientFor(instances[1])
	bucket := fmt.Sprintf("pb-side-by-side-%d", suffix)
	if err := a.Bucket(bucket).Create(ctx, projectA, nil); err != nil {
		t.Fatalf("create bucket on %s: %v", instances[0].name, err)
	}
	if !list(a, projectA)[bucket] {
		t.Errorf("%s does not list the bucket just created on it", instances[0].name)
	}
	// Two instances, not one reached twice: b answers on its own port and
	// has none of a's state.
	_ = list(b, projectB) // answers on its own port
	if list(b, projectA)[bucket] {
		t.Errorf("%s lists a bucket created on %s: both ports reach one instance", instances[1].name, instances[0].name)
	}
}
