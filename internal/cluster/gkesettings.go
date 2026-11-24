package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Cluster settings of the Kubernetes Engine API's clusters (#2100), each
// with the effect it has on GKE, made on the kind cluster:
//
//   - legacy ABAC: the API server's ABAC authorizer, after RBAC, with the
//     legacy policy (abacPolicy);
//   - network policy enforcement: kindnet's NetworkPolicy controller,
//     off unless it is enabled (SetNetworkPolicy), as a GKE Standard
//     cluster enforces no NetworkPolicy until network policy is enabled;
//   - node repair: a node is replaced by one of the same name.

// abacPolicyFile is where the ABAC policy is kept on the control plane:
// under /etc/kubernetes/pki, which kubeadm's API server pod already mounts.
const abacPolicyFile = "/etc/kubernetes/pki/abac-authz-policy.jsonl"

// abacPolicy is the legacy authorization policy: every user may read the
// API's non-resource paths, and the service accounts may do everything.
// It is the policy Kubernetes' own GCE cluster scripts installed with
// legacy ABAC (cluster/gce/manifests/abac-authz-policy.jsonl); GKE's own
// is not published. RBAC is consulted first, as GKE does ("To authorize
// an action, GKE checks for an RBAC policy first", re-read 2026-11-21:
// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/role-based-access-control).
const abacPolicy = `{"apiVersion":"abac.authorization.kubernetes.io/v1beta1","kind":"Policy","spec":{"user":"*","nonResourcePath":"*","readonly":true}}
{"apiVersion":"abac.authorization.kubernetes.io/v1beta1","kind":"Policy","spec":{"group":"system:serviceaccounts","namespace":"*","resource":"*","apiGroup":"*","nonResourcePath":"*"}}
`

// abacScript turns the API server's ABAC authorizer on ($1 = on) or off in
// its static pod manifest; the kubelet then restarts the API server.
const abacScript = `set -e
m=/etc/kubernetes/manifests/kube-apiserver.yaml
f=` + abacPolicyFile + `
grep -q -- '--authorization-mode=' "$m" || { echo "the API server has no --authorization-mode flag" >&2; exit 1; }
sed -i -e '/--authorization-policy-file=/d' -e 's/--authorization-mode=\(.*\),ABAC$/--authorization-mode=\1/' "$m"
if [ "$1" = on ]; then
  printf '%s' "$2" > "$f"
  sed -i -e 's/^\( *\)- --authorization-mode=\(.*\)$/\1- --authorization-mode=\2,ABAC\n\1- --authorization-policy-file=\/etc\/kubernetes\/pki\/abac-authz-policy.jsonl/' "$m"
else
  rm -f "$f"
fi
`

// apiServerArgs is the running API server's command line, read from /proc
// on the control plane.
const apiServerArgs = `for p in /proc/[0-9]*; do a=$(tr '\0' ' ' < "$p/cmdline" 2>/dev/null) || continue; case "$a" in kube-apiserver\ *) echo "$a";; esac; done`

// SetLegacyAbac turns the API server's legacy ABAC authorizer on or off,
// and waits for the API server to serve with it.
func (g *GKE) SetLegacyAbac(ctx context.Context, kind string, enabled bool) error {
	if !strings.HasPrefix(kind, OwnerPrefix) {
		return fmt.Errorf("%w: %q", ErrNotOwned, kind)
	}
	r := g.runner()
	cp := kind + "-control-plane"
	mode := "off"
	if enabled {
		mode = "on"
	}
	if _, err := r.Run(ctx, "docker", "exec", cp, "sh", "-c", abacScript, "sh", mode, abacPolicy); err != nil {
		return fmt.Errorf("rewrite the API server's authorization flags: %w", err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		args, _ := r.Run(ctx, "docker", "exec", cp, "sh", "-c", apiServerArgs)
		has := strings.Contains(args, "--authorization-policy-file=")
		if strings.TrimSpace(args) != "" && has == enabled {
			if _, err := r.Run(ctx, "kubectl", "--kubeconfig", g.kubeconfig(kind), "get", "--raw", "/readyz"); err == nil {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return errors.New("the API server did not restart with its new authorization flags")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// kindnetRole is kindnet's ClusterRole, as the node image's
// default-cni.yaml makes it, with or without its NetworkPolicy rule.
func kindnetRole(policies bool) string {
	rule := ""
	if policies {
		rule = `,{"apiGroups":["networking.k8s.io"],"resources":["networkpolicies"],"verbs":["list","watch"]}`
	}
	return `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"kindnet"},"rules":[` +
		`{"apiGroups":["policy"],"resources":["podsecuritypolicies"],"verbs":["use"],"resourceNames":["kindnet"]},` +
		`{"apiGroups":[""],"resources":["nodes","pods","namespaces"],"verbs":["list","watch"]}` + rule + `]}`
}

// SetNetworkPolicy turns NetworkPolicy enforcement on or off on every
// node. kindnet always runs its NetworkPolicy controller
// (kube-network-policies) and has no flag to stop it; without the right
// to list NetworkPolicies it sees none, so it selects no pod, and its
// nftables rules queue nothing (their queue is "bypass": with no
// listener, packets pass). Enforcement is that right, given or taken
// away, and kindnet restarted to see the change.
//
// GKE re-creates the nodes to make the change ("Enabling network policy
// enforcement requires that your nodes be re-created", re-read
// 2026-11-24: https://docs.cloud.google.com/kubernetes-engine/docs/how-to/network-policy),
// so no connection made before it survives it. kindnet's rules accept a
// connection's packets once it is established, and queue only new ones
// to its policy check: a connection kept alive across the change (the API
// server's service proxy keeps its connections to pods for 90 seconds of
// idleness) would be let through by a deny-all policy just enabled (main
// run 45422957668). Every node's connection tracking is flushed once
// kindnet runs with the change, as new nodes start without any.
func (g *GKE) SetNetworkPolicy(ctx context.Context, kind string, enabled bool) error {
	if !strings.HasPrefix(kind, OwnerPrefix) {
		return fmt.Errorf("%w: %q", ErrNotOwned, kind)
	}
	r := g.runner()
	kc := g.kubeconfig(kind)
	role := filepath.Join(g.Dir, kind, "kindnet-role.json")
	if err := os.WriteFile(role, []byte(kindnetRole(enabled)), 0o600); err != nil {
		return err
	}
	if _, err := r.Run(ctx, "kubectl", "--kubeconfig", kc, "replace", "-f", role); err != nil {
		return fmt.Errorf("set kindnet's NetworkPolicy access: %w", err)
	}
	if _, err := r.Run(ctx, "kubectl", "--kubeconfig", kc, "-n", "kube-system", "rollout", "restart", "daemonset", "kindnet"); err != nil {
		return fmt.Errorf("restart kindnet: %w", err)
	}
	if _, err := r.Run(ctx, "kubectl", "--kubeconfig", kc, "-n", "kube-system", "rollout", "status", "daemonset", "kindnet", "--timeout=180s"); err != nil {
		return fmt.Errorf("kindnet did not roll out: %w", err)
	}
	nodes, err := g.clusterNodes(ctx, kind)
	if err != nil {
		return fmt.Errorf("list the nodes: %w", err)
	}
	for _, n := range nodes {
		if _, err := r.Run(ctx, "docker", "exec", n, "conntrack", "-F"); err != nil {
			return fmt.Errorf("flush node %s's connection tracking: %w", n, err)
		}
	}
	return nil
}

// Get is a GET of the cluster's Kubernetes API at path (kubectl get --raw).
func (g *GKE) Get(ctx context.Context, kind, path string) ([]byte, error) {
	out, err := g.runner().Run(ctx, "kubectl", "--kubeconfig", g.kubeconfig(kind), "get", "--raw", path)
	return []byte(out), err
}

// RepairNode replaces a pool's node by a new one of the same name and
// image: it is drained (PodDisruptionBudgets respected), removed and
// joined again, as GKE's auto-repair recreates a node. A node that does
// not answer cannot finish a drain, so the drain is given two minutes.
func (g *GKE) RepairNode(ctx context.Context, kind, image, pool, name string) error {
	if !strings.HasPrefix(name, kind+"-") {
		return fmt.Errorf("%w: node %q is not cluster %q's", ErrNotOwned, name, kind)
	}
	dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	_ = g.ForceDrainNode(dctx, kind, name)
	cancel()
	if err := g.RemoveNode(ctx, kind, name); err != nil {
		return err
	}
	return g.join(ctx, kind, image, pool, name)
}
