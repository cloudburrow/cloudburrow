package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/cluster"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/images"
	"github.com/cloudburrow/cloudburrow/internal/prefetch"
	"github.com/cloudburrow/cloudburrow/internal/storageimage"
)

// offlinePlan is what `up` with this configuration downloads (#604): the
// node image, the storage image this CLI builds, the enabled backends'
// images, and Knative's YAMLs when Cloud Run is enabled.
func offlinePlan(ctx context.Context, cfg config.Config, r prefetch.Runner) prefetch.Plan {
	plan := prefetch.Plan{NodeImage: cfg.Cluster.NodeImage, Images: map[string][]string{}}
	comps := components.NewLifecycleComponent(cfg.KubeconfigPath(), cfg, io.Discard)
	for _, b := range comps.Backends() {
		if b.Name == "storage" {
			continue // built by this CLI, below
		}
		if b.Image != "" && !images.IsLocal(b.Image) {
			plan.Images[b.Image] = append(plan.Images[b.Image], b.Name)
		}
	}
	plan.Arch = daemonArch(ctx, r)
	if serviceEnabled(cfg, config.ServiceStorage) {
		plan.Storage = true
		if bin, err := storageimage.Binary(plan.Arch); err == nil {
			plan.StorageImage = storageimage.Tag(bin)
		}
	}
	if comps.NeedsKnative() {
		for _, m := range components.KnativeManifests() {
			plan.Knative = append(plan.Knative, prefetch.KnativeManifest{Name: m.Name, URL: m.URL, SHA256: m.SHA256})
		}
	}
	return plan
}

// daemonArch is the Docker daemon's architecture, which is a kind node's.
// It falls back to this machine's when the daemon does not answer; the
// storage image's tag then names that architecture, which is right on
// every host where Docker runs natively or in a VM of the same kind.
func daemonArch(ctx context.Context, r prefetch.Runner) string {
	if out, err := r.Run(ctx, "docker", "version", "--format", "{{.Server.Arch}}"); err == nil {
		if a := strings.TrimSpace(out); a != "" {
			return a
		}
	}
	return runtime.GOARCH
}

// runPrefetch implements `cloudburrow prefetch`: it stores in the state
// directory everything `up` with the same flags would download.
func runPrefetch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := config.Load(config.Options{Args: args, Output: stderr})
	if err != nil {
		return err
	}
	r := prefetch.ExecRunner{}
	plan := offlinePlan(ctx, cfg, r)
	if plan.Storage && plan.StorageImage == "" {
		return fmt.Errorf("the builtin storage image: %w", storageimage.ErrNotEmbedded)
	}
	cache := prefetch.Cache{Dir: prefetch.CacheDir(cfg.StateDir)}
	fmt.Fprintf(stdout, "prefetching into %s for services %s\n", cache.Dir, servicesList(cfg))
	p := &prefetch.Prefetcher{
		Cache:  cache,
		Runner: r,
		Fetch:  components.FetchManifest,
		BuildStorage: func(ctx context.Context) (string, error) {
			return storageimage.Build(ctx, images.ExecRunner{}, plan.Arch)
		},
		Node:     throwawayNode(cfg.Cluster.NodeImage),
		Platform: "linux/" + plan.Arch,
		Out:      stdout,
	}
	stored, err := p.Run(ctx, plan)
	if err != nil {
		return err
	}
	printStored(stdout, stored)
	return nil
}

// throwawayNode starts a kind cluster of its own, under a name no instance
// uses and with a kubeconfig in a temporary directory, for prefetch to pull
// node images with; stop deletes it.
func throwawayNode(nodeImage string) func(ctx context.Context) (string, func(), error) {
	return func(ctx context.Context) (string, func(), error) {
		suffix := make([]byte, 4)
		if _, err := rand.Read(suffix); err != nil {
			return "", nil, err
		}
		dir, err := os.MkdirTemp("", "cloudburrow-prefetch-")
		if err != nil {
			return "", nil, err
		}
		name := "cloudburrow-prefetch-" + hex.EncodeToString(suffix)
		c, err := cluster.New(cluster.Options{Name: name, NodeImage: nodeImage, Kubeconfig: filepath.Join(dir, "kubeconfig")})
		if err != nil {
			_ = os.RemoveAll(dir)
			return "", nil, err
		}
		stop := func() {
			// Not the caller's context: an interrupted prefetch still
			// removes its cluster.
			dctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			_ = c.Delete(dctx)
			_ = os.RemoveAll(dir)
		}
		if _, err := c.Ensure(ctx, ""); err != nil {
			stop()
			return "", nil, describeClusterError(err)
		}
		nodes, err := (&images.Loader{ClusterName: name, Runner: images.ExecRunner{}}).Nodes(ctx)
		if err != nil {
			stop()
			return "", nil, err
		}
		return nodes[0], stop, nil
	}
}

func servicesList(cfg config.Config) string {
	var names []string
	for _, s := range cfg.EnabledServices() {
		names = append(names, string(s))
	}
	return strings.Join(names, ",")
}

// printStored lists what the cache holds for this configuration, with sizes,
// so what is copied to an air-gapped machine is known.
func printStored(w io.Writer, stored []prefetch.Stored) {
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SIZE\tKIND\tARTIFACT\tFOR")
	var total int64
	for _, s := range stored {
		total += s.Bytes
		note := s.What
		if s.Unpinned {
			note += " (named by tag in the pinned YAML, not by digest)"
		}
		if !s.Fresh {
			note += " (already cached)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", mib(s.Bytes), s.Kind, s.Ref, note)
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\n%d artifacts, %s in total. Copy the state directory to use them offline: `cloudburrow up --offline`.\n",
		len(stored), mib(total))
}

func mib(b int64) string {
	return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
}

// offlineFlag takes up's -offline from the arguments.
func offlineFlag(args []string) (bool, []string, error) {
	v, found, rest, err := splitFlag(args, "offline", true)
	if err != nil || !found {
		return false, rest, err
	}
	switch v {
	case "true", "1", "":
		return true, rest, nil
	case "false", "0":
		return false, rest, nil
	}
	return false, nil, fmt.Errorf("invalid -offline %q", v)
}

// offlineCache is how `up` uses the cache: the plan, and the artifacts it
// resolves to.
type offlineCache struct {
	cache   prefetch.Cache
	plan    prefetch.Plan
	arts    []prefetch.Artifact
	offline bool
	runner  prefetch.Runner
	out     io.Writer
	// nodes imports into the cluster; set by up.
	nodes *images.Loader
}

// newOfflineCache resolves the plan against the cache. With offline set it
// refuses, before anything is created, when an artifact is missing.
func newOfflineCache(ctx context.Context, cfg config.Config, offline bool, r prefetch.Runner, out io.Writer) (*offlineCache, error) {
	cache := prefetch.Cache{Dir: prefetch.CacheDir(cfg.StateDir)}
	plan := offlinePlan(ctx, cfg, r)
	oc := &offlineCache{cache: cache, plan: plan, arts: cache.Artifacts(plan), offline: offline, runner: r, out: out}
	if offline {
		if a, missing := cache.FirstMissing(oc.arts); missing {
			return nil, fmt.Errorf("up --offline: %w", cache.MissingError(a))
		}
	}
	return oc, nil
}

// use makes the installer read Knative's YAMLs from the cache, and points
// the node import at the instance's cluster.
func (o *offlineCache) use(comps *components.LifecycleComponent, cfg config.Config) {
	comps.Installer().Fetch = o.cache.Fetcher(o.plan, components.FetchManifest, o.offline)
	o.nodes = &images.Loader{ClusterName: cfg.ClusterName(), Runner: images.ExecRunner{}}
}

func (o *offlineCache) loader() *prefetch.Loader {
	return &prefetch.Loader{Cache: o.cache, Runner: o.runner, Nodes: o.nodes, Out: o.out}
}

// offlineHostComponent loads the cached node and storage images into
// Docker before the cluster is created, so kind finds the node image and
// the storage image is not rebuilt from a base it would have to pull.
type offlineHostComponent struct{ c *offlineCache }

func (h offlineHostComponent) Name() string { return "offline-cache-host" }
func (h offlineHostComponent) Start(ctx context.Context) error {
	return h.c.loader().LoadHost(ctx, h.c.arts)
}
func (h offlineHostComponent) Stop(context.Context) error { return nil }

// offlineNodesComponent imports the cached backend and Knative images into
// the cluster's nodes before anything that runs them is applied.
type offlineNodesComponent struct{ c *offlineCache }

func (n offlineNodesComponent) Name() string { return "offline-cache-nodes" }
func (n offlineNodesComponent) Start(ctx context.Context) error {
	if n.c.nodes == nil {
		return errors.New("offline cache: no cluster to import into")
	}
	return n.c.loader().LoadNodes(ctx, n.c.arts)
}
func (n offlineNodesComponent) Stop(context.Context) error { return nil }
