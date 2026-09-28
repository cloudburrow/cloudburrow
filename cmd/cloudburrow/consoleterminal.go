package main

import (
	"context"
	"sort"
	"strings"

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
func newConsoleTerminal(cfg config.Config, addrs func() map[string]string) console.Terminal {
	kubeconfig := cfg.KubeconfigPath()
	return consoleTerminal{terminal.New(terminal.Config{
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
