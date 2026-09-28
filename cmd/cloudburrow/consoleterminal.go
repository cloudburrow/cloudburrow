package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/terminal"
)

// The console's terminal (#781) is a pod in the instance's cluster. This
// file is its wiring: the pod's environment, which is the same pod
// environment a Cloud Run revision is given plus gcloud-setup's
// configuration at the addresses a pod reaches, and the adapter from
// internal/terminal to the console's narrow interface.

// podGcloudEnv is `gcloud-setup`'s configuration as a pod's environment:
// the endpoint override of every service gcloudVerified names that a pod
// can reach at inCluster, auth/disable_credentials, and no credential file.
// gcloud reads CLOUDSDK_<SECTION>_<PROPERTY> as it reads the configuration
// file, so the pod needs no file written into it.
//
// A service with no in-cluster address is left out rather than pointed at
// a host address: the pod cannot reach loopback, and a missing override
// sends that one command to Google's endpoint, where, with credentials
// disabled, it is refused as unauthenticated.
func podGcloudEnv(inCluster map[string]string) []envVar {
	vars := []envVar{
		{"CLOUDSDK_AUTH_DISABLE_CREDENTIALS", "true", "CloudBurrow authenticates nothing; no credential is sent"},
		{"CLOUDSDK_CORE_DISABLE_USAGE_REPORTING", "true", ""},
		{"CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK", "true", ""},
	}
	for _, v := range gcloudVerified {
		addr := inCluster[string(v.service)]
		if !isInClusterAddr(addr) {
			continue
		}
		vars = append(vars, envVar{"CLOUDSDK_API_ENDPOINT_OVERRIDES_" + strings.ToUpper(v.property),
			"http://" + addr + v.path, "gcloud " + v.property + " at this instance"})
	}
	sort.Slice(vars, func(i, j int) bool { return vars[i].Name < vars[j].Name })
	return vars
}

// terminalEnv is the terminal pod's whole environment.
func terminalEnv(inCluster map[string]string) map[string]string {
	env := podEnvMap(inCluster)
	for _, v := range podGcloudEnv(inCluster) {
		env[v.Name] = v.Value
	}
	return env
}

// newConsoleTerminal is the terminal the console's drawer opens, reading
// the pod addresses when the pod is made, since the CLI-hosted services'
// are known only once cloudburrow-host has started.
// warm is the background import of the image from the offline cache, which
// the terminal waits for rather than racing it with a pull.
func newConsoleTerminal(cfg config.Config, addrs func() map[string]string, warm *terminalWarm) console.Terminal {
	kubeconfig := cfg.KubeconfigPath()
	return consoleTerminal{terminal.New(terminal.Config{
		Before:   warm.wait,
		Kube:     k8s.New(kubeconfig, "", cfg.Cluster.Namespace),
		KubeIn:   func(ns string) *k8s.Runner { return k8s.New(kubeconfig, "", ns) },
		Instance: cfg.Name,
		// kubectl in the pod reads the instance's backends and the Cloud
		// Run workloads, and writes nothing.
		ViewNamespaces: []string{cfg.Cluster.Namespace, runadapter.WorkloadNamespace},
		Env:            func() map[string]string { return terminalEnv(addrs()) },
	})}
}

// consoleTerminal adapts terminal.Manager to console.Terminal.
type consoleTerminal struct{ m *terminal.Manager }

func (c consoleTerminal) Prepare(ctx context.Context, progress func(string)) error {
	return c.m.Prepare(ctx, progress)
}

func (c consoleTerminal) Open(project string, cols, rows uint16) (console.TerminalSession, error) {
	s, err := c.m.Open(project, cols, rows)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// terminalWarm is `up`'s background import of the terminal image from the
// offline cache into the cluster's node (#824). The image is about 1 GB
// compressed and only the terminal drawer uses it, so it is not imported
// before anything starts, as the other cached images are; nor is it pulled
// when it is not cached, which would cost every `up` that download whether
// or not the terminal is ever opened.
type terminalWarm struct {
	mu      sync.Mutex
	done    chan struct{}
	started time.Time
	err     error
}

// start imports in the background until load returns or ctx, `up`'s own,
// is cancelled.
func (w *terminalWarm) start(ctx context.Context, load func(context.Context) error, out io.Writer) {
	w.mu.Lock()
	w.done = make(chan struct{})
	w.started = time.Now()
	done := w.done
	w.mu.Unlock()
	fmt.Fprintln(out, "  offline cache: importing the console terminal image in the background")
	go func() {
		err := load(ctx)
		w.mu.Lock()
		w.err = err
		w.mu.Unlock()
		switch {
		case err != nil && ctx.Err() == nil:
			fmt.Fprintf(out, "warning: the console terminal image was not imported from the offline cache: %v; "+
				"the cluster pulls it when the terminal is first opened\n", err)
		case err == nil:
			fmt.Fprintln(out, "  offline cache: console terminal image imported")
		}
		close(done)
	}()
}

// wait holds the terminal's first use until an import under way is done.
// A failed import is not the terminal's failure: the kubelet pulls the
// image instead, and the drawer shows that pull.
func (w *terminalWarm) wait(ctx context.Context, progress func(string)) error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	done, started := w.done, w.started
	w.mu.Unlock()
	if done == nil {
		return nil
	}
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return nil
		default:
		}
		progress("Importing the terminal image from the offline cache into the cluster's node: " +
			time.Since(started).Truncate(time.Second).String() + " so far")
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
