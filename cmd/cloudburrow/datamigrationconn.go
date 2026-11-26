package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/dmstunnel"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/service/datamigration"
)

// A source's forward SSH tunnel (#2257): a Deployment of
// `cloudburrow-storage dms-tunnel` in the instance's namespace, with its
// credentials in a Secret and a Service the destinations' servers reach
// the source by. One per source profile, made when a destination first
// reaches the source and deleted with the profile (Release).

// dmsTunnelPort is the port the tunnel's Service listens on.
const dmsTunnelPort = 5000

// dmsTunnelName is a source profile's tunnel's name.
func dmsTunnelName(profile string) string {
	return fmt.Sprintf("dms-tunnel-%08x", crc32.ChecksumIEEE([]byte(profile)))
}

// dmsTunnelManifest is the tunnel's Secret, Deployment and Service, as a
// List kubectl applies.
func dmsTunnelManifest(name, namespace, image string, src datamigration.Source) (string, error) {
	t := src.SSH
	labels := map[string]string{"app": name, "app.kubernetes.io/managed-by": "cloudburrow", "cloudburrow.dev/component": "dms-tunnel"}
	secret := map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]any{"name": name, "namespace": namespace, "labels": labels},
		"stringData": map[string]string{dmstunnel.EnvPassword: t.Password, dmstunnel.EnvPrivateKey: t.PrivateKey}}
	env := []map[string]any{}
	for _, k := range []string{dmstunnel.EnvPassword, dmstunnel.EnvPrivateKey} {
		env = append(env, map[string]any{"name": k, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": name, "key": k}}})
	}
	args := []string{"dms-tunnel", "--listen", "0.0.0.0:" + strconv.Itoa(dmsTunnelPort),
		"--target", net.JoinHostPort(src.Host, strconv.Itoa(src.Port)),
		"--ssh", net.JoinHostPort(t.Host, strconv.Itoa(t.Port)), "--ssh-user", t.User}
	deploy := map[string]any{"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": name, "namespace": namespace, "labels": labels},
		"spec": map[string]any{"replicas": 1, "selector": map[string]any{"matchLabels": map[string]string{"app": name}},
			"template": map[string]any{"metadata": map[string]any{"labels": labels,
				// A changed profile rolls the tunnel.
				"annotations": map[string]string{"cloudburrow.dev/config": fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(strings.Join(append(args, t.Password, t.PrivateKey), "\x00"))))}},
				"spec": map[string]any{"containers": []map[string]any{{
					"name": "tunnel", "image": image, "imagePullPolicy": "Never", "args": args, "env": env,
					"ports":          []map[string]any{{"containerPort": dmsTunnelPort}},
					"readinessProbe": map[string]any{"tcpSocket": map[string]any{"port": dmsTunnelPort}, "periodSeconds": 2},
					"resources":      map[string]any{"requests": map[string]string{"memory": "16Mi"}, "limits": map[string]string{"memory": "64Mi"}},
				}}}}}}
	svc := map[string]any{"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": name, "namespace": namespace, "labels": labels},
		"spec": map[string]any{"selector": map[string]string{"app": name},
			"ports": []map[string]any{{"port": dmsTunnelPort, "targetPort": dmsTunnelPort}}}}
	b, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{secret, deploy, svc}})
	return string(b), err
}

// reach is src as the destinations' servers reach it: through its tunnel,
// started (or updated) and ready, when it has forward SSH connectivity.
func (b *dmsBackend) reach(ctx context.Context, src datamigration.Source) (datamigration.Source, error) {
	if src.SSH == nil {
		return src, nil
	}
	if sqlThreadPoolImage == nil {
		return src, fmt.Errorf("the SSH tunnel to %s runs from the cloudburrow-storage image, which this instance cannot build", src.SSH.Host)
	}
	image, err := sqlThreadPoolImage(ctx)
	if err != nil {
		return src, fmt.Errorf("the SSH tunnel's image: %w", err)
	}
	name := dmsTunnelName(src.Name)
	m, err := dmsTunnelManifest(name, b.cfg.Cluster.Namespace, image, src)
	if err != nil {
		return src, err
	}
	kube := kubeRunner(b.cfg.KubeconfigPath(), b.cfg.Cluster.Namespace)
	if err := kube.Apply(ctx, m, k8s.ApplyOptions{}); err != nil {
		return src, fmt.Errorf("start the SSH tunnel to %s: %w", src.SSH.Host, kubectlCause(err))
	}
	if _, err := kube.Do(ctx, "", "rollout", "status", "deployment/"+name, "--timeout=180s"); err != nil {
		return src, fmt.Errorf("the SSH tunnel to %s did not become ready: %w", src.SSH.Host, kubectlCause(err))
	}
	// A ready pod is not yet a Service that answers: its EndpointSlice and
	// the nodes' forwarding rules follow it (a first connection right after
	// the rollout was refused), so wait for a ready endpoint; the callers
	// retry a refused connection for the rest (tunnelRetry).
	for deadline := time.Now().Add(time.Minute); ; {
		out, err := kube.Do(ctx, "", "get", "endpointslices", "-l", "kubernetes.io/service-name="+name,
			"-o", `jsonpath={.items[*].endpoints[?(@.conditions.ready==true)].addresses[0]}`)
		if err == nil && strings.TrimSpace(out) != "" {
			break
		}
		if time.Now().After(deadline) {
			return src, fmt.Errorf("the SSH tunnel to %s has no ready endpoint", src.SSH.Host)
		}
		select {
		case <-ctx.Done():
			return src, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	out := src
	out.Host, out.Port, out.SSH = name+"."+b.cfg.Cluster.Namespace+".svc.cluster.local", dmsTunnelPort, nil
	return out, nil
}

func (b *dmsBackend) Release(ctx context.Context, src datamigration.Source) error {
	if src.SSH == nil {
		return nil
	}
	kube := kubeRunner(b.cfg.KubeconfigPath(), b.cfg.Cluster.Namespace)
	name := dmsTunnelName(src.Name)
	for _, r := range []string{"service", "deployment", "secret"} {
		if err := kube.Delete(ctx, r, name, true); err != nil {
			return kubectlCause(err)
		}
	}
	return nil
}

// tlsStdin is the input QuerySource's script reads after the password:
// the source's CA certificate, client certificate and key, each base64
// on a line of its own ("" for none).
func tlsStdin(src datamigration.Source) string {
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	return enc(src.CACert) + "\n" + enc(src.ClientCert) + "\n" + enc(src.ClientKey) + "\n"
}

// tlsFiles is the script prelude that writes tlsStdin's certificates to a
// temporary directory: $ca, $cert and $key name them, "" for none.
const tlsFiles = `tlsdir=$(mktemp -d); trap 'rm -rf "$tlsdir"' EXIT
ca=""; cert=""; key=""
IFS= read -r l; if [ -n "$l" ]; then printf '%s' "$l" | base64 -d > "$tlsdir/ca.pem"; ca="$tlsdir/ca.pem"; fi
IFS= read -r l; if [ -n "$l" ]; then printf '%s' "$l" | base64 -d > "$tlsdir/cert.pem"; cert="$tlsdir/cert.pem"; fi
IFS= read -r l; if [ -n "$l" ]; then (umask 077; printf '%s' "$l" | base64 -d > "$tlsdir/key.pem"); key="$tlsdir/key.pem"; fi
`

// tunnelRefusedRE is a client's refused or failed connection, which, to a
// tunnel just made, is the Service's forwarding not yet in place.
var tunnelRefusedRE = regexp.MustCompile(`Connection refused|Can't connect to MySQL server|Lost connection to MySQL server at 'reading initial`)

// tunnelRetry runs f, again while a tunnelled source's connection is
// refused, for up to 30 seconds; a source with no tunnel runs once.
func tunnelRetry(ctx context.Context, tunnelled bool, f func() error) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := f()
		if err == nil || !tunnelled || !tunnelRefusedRE.MatchString(err.Error()) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Second):
		}
	}
}
