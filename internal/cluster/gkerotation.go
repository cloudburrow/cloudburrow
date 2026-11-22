package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Credential rotation of the Kubernetes Engine API's clusters (#2100), as
// GKE rotates a cluster's root CA (re-read 2026-11-21:
// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/credential-rotation):
//
//   - Start makes a new cluster CA on the control plane (openssl, which the
//     kind node image has) and a copy of it signed by the old CA. The API
//     server's serving certificate is issued by the new CA and served with
//     that copy as its intermediate, so a client that trusts either CA
//     verifies it; the API server, the controller manager and every node's
//     kubelet trust client certificates of both CAs; the controller manager
//     signs with the new one; the cluster-info a joining node reads holds
//     both. The admin credentials are made again from the new CA.
//   - Complete stops trusting the old CA: the serving certificate loses
//     its old-CA intermediate, the control plane's and every node's
//     credentials are re-issued from the new CA, the bundles hold only the
//     new CA, and the pods are re-created (GKE re-creates the nodes), so
//     every in-cluster client reads the new CA.
//
// The key files never leave the control plane except a node's kubelet key,
// copied from the control plane to its node through the Runner.

const pki = "/etc/kubernetes/pki"

// rotateStartScript makes the new CA and switches the control plane to it,
// trusting both. $1 is the new CA's common name.
const rotateStartScript = `set -e
cd ` + pki + `
[ ! -f ca-old.crt ] || { echo "a credential rotation is already in progress" >&2; exit 1; }
cp ca.crt ca-old.crt
cp ca.key ca-old.key
printf 'basicConstraints=critical,CA:TRUE\nkeyUsage=critical,digitalSignature,keyEncipherment,keyCertSign\nsubjectKeyIdentifier=hash\n' > ca-ext.cnf
openssl genrsa -out ca-new.key 2048 2>/dev/null
openssl req -x509 -new -key ca-new.key -sha256 -days 3650 -subj "/CN=$1" -out ca-new.crt
openssl req -new -key ca-new.key -subj "/CN=$1" -out ca-cross.csr
openssl x509 -req -in ca-cross.csr -CA ca-old.crt -CAkey ca-old.key -CAcreateserial -days 3650 -sha256 -extfile ca-ext.cnf -out ca-cross.crt 2>/dev/null
rm -f ca-cross.csr ca-old.srl ca-ext.cnf
cat ca-new.crt ca-old.crt > ca-trust.crt
mv ca-new.crt ca.crt
mv ca-new.key ca.key
rm -f apiserver.crt apiserver.key apiserver-kubelet-client.crt apiserver-kubelet-client.key
kubeadm init phase certs apiserver --config /kind/kubeadm.conf >/dev/null
kubeadm init phase certs apiserver-kubelet-client --config /kind/kubeadm.conf >/dev/null
cat ca-cross.crt >> apiserver.crt
cd /etc/kubernetes
rm -f admin.conf super-admin.conf
kubeadm init phase kubeconfig admin --config /kind/kubeadm.conf >/dev/null
kubeadm init phase kubeconfig super-admin --config /kind/kubeadm.conf >/dev/null
sed -i -e 's#--client-ca-file=` + pki + `/ca.crt#--client-ca-file=` + pki + `/ca-trust.crt#' manifests/kube-apiserver.yaml
sed -i -e 's#--client-ca-file=` + pki + `/ca.crt#--client-ca-file=` + pki + `/ca-trust.crt#' -e 's#--root-ca-file=` + pki + `/ca.crt#--root-ca-file=` + pki + `/ca-trust.crt#' manifests/kube-controller-manager.yaml
`

// rotateCompleteScript drops the old CA on the control plane: the serving
// certificate without its old-CA intermediate, the components'
// kubeconfigs re-issued, the bundles back to the new CA alone.
const rotateCompleteScript = `set -e
cd ` + pki + `
[ -f ca-old.crt ] || { echo "no credential rotation is in progress" >&2; exit 1; }
rm -f apiserver.crt apiserver.key
kubeadm init phase certs apiserver --config /kind/kubeadm.conf >/dev/null
cd /etc/kubernetes
rm -f controller-manager.conf scheduler.conf
kubeadm init phase kubeconfig controller-manager --config /kind/kubeadm.conf >/dev/null
kubeadm init phase kubeconfig scheduler --config /kind/kubeadm.conf >/dev/null
sed -i -e 's#` + pki + `/ca-trust.crt#` + pki + `/ca.crt#g' manifests/kube-apiserver.yaml manifests/kube-controller-manager.yaml
rm -f ` + pki + `/ca-old.crt ` + pki + `/ca-old.key ` + pki + `/ca-trust.crt ` + pki + `/ca-cross.crt
`

// kubeletCertScript issues a node's kubelet client certificate from the
// control plane's CA: $1 is the node's name. It prints the key and the
// certificate, the kubelet's one PEM file.
const kubeletCertScript = `set -e
d=$(mktemp -d)
openssl genrsa -out "$d/k.key" 2048 2>/dev/null
openssl req -new -key "$d/k.key" -subj "/O=system:nodes/CN=system:node:$1" -out "$d/k.csr"
printf 'keyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=clientAuth\n' > "$d/ext.cnf"
openssl x509 -req -in "$d/k.csr" -CA ` + pki + `/ca.crt -CAkey ` + pki + `/ca.key -CAcreateserial -CAserial "$d/ca.srl" -days 365 -sha256 -extfile "$d/ext.cnf" -out "$d/k.crt" 2>/dev/null
cat "$d/k.key" "$d/k.crt"
rm -rf "$d"
`

// nodeTrustScript makes a node trust $1 (a PEM bundle) for client
// certificates and the API server, with $2 its kubelet's new client
// credentials when not empty, and restarts the kubelet.
const nodeTrustScript = `set -e
printf '%s' "$1" > ` + pki + `/ca.crt
if [ -n "$2" ]; then
  f=/var/lib/kubelet/pki/kubelet-client-rotated.pem
  printf '%s' "$2" > "$f"
  chmod 600 "$f"
  ln -sf "$f" /var/lib/kubelet/pki/kubelet-client-current.pem
  ca=$(printf '%s' "$1" | base64 -w0)
  sed -i -e "s#certificate-authority-data: .*#certificate-authority-data: $ca#" /etc/kubernetes/kubelet.conf
fi
systemctl restart kubelet
`

// clusterNodes lists every node container of kind's cluster.
func (g *GKE) clusterNodes(ctx context.Context, kind string) ([]string, error) {
	out, err := g.runner().Run(ctx, "docker", "ps", "--filter", "label=io.x-k8s.kind.cluster="+kind, "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// cpFile is a file of the control plane, read through the Runner.
func (g *GKE) cpFile(ctx context.Context, kind, path string) (string, error) {
	return g.runner().Run(ctx, "docker", "exec", kind+"-control-plane", "cat", path)
}

// StartCredentialRotation starts rotating kind's cluster CA and returns
// the cluster's new access (the admin credentials re-issued from the new
// CA); both CAs are trusted until CompleteCredentialRotation.
func (g *GKE) StartCredentialRotation(ctx context.Context, kind string) (GKEAccess, error) {
	if !strings.HasPrefix(kind, OwnerPrefix) {
		return GKEAccess{}, fmt.Errorf("%w: %q", ErrNotOwned, kind)
	}
	r := g.runner()
	cp := kind + "-control-plane"
	if _, err := r.Run(ctx, "docker", "exec", cp, "sh", "-c", rotateStartScript, "sh", fmt.Sprintf("kubernetes-%d", time.Now().Unix())); err != nil {
		return GKEAccess{}, fmt.Errorf("make the new cluster CA: %w", err)
	}
	trust, err := g.cpFile(ctx, kind, pki+"/ca-trust.crt")
	if err != nil {
		return GKEAccess{}, err
	}
	if err := g.trustOnNodes(ctx, kind, trust, false); err != nil {
		return GKEAccess{}, err
	}
	if err := g.waitAPIServer(ctx, kind, "ca-trust.crt"); err != nil {
		return GKEAccess{}, err
	}
	if err := g.setClusterInfoCA(ctx, kind, trust); err != nil {
		return GKEAccess{}, err
	}
	if err := g.openNextEndpoint(ctx, kind); err != nil {
		return GKEAccess{}, err
	}
	return g.exportAccess(ctx, kind)
}

// CompleteCredentialRotation stops trusting the old CA and returns the
// cluster's access.
func (g *GKE) CompleteCredentialRotation(ctx context.Context, kind string) (GKEAccess, error) {
	if !strings.HasPrefix(kind, OwnerPrefix) {
		return GKEAccess{}, fmt.Errorf("%w: %q", ErrNotOwned, kind)
	}
	r := g.runner()
	cp := kind + "-control-plane"
	if _, err := r.Run(ctx, "docker", "exec", cp, "sh", "-c", rotateCompleteScript); err != nil {
		return GKEAccess{}, fmt.Errorf("drop the old cluster CA: %w", err)
	}
	ca, err := g.cpFile(ctx, kind, pki+"/ca.crt")
	if err != nil {
		return GKEAccess{}, err
	}
	if err := g.trustOnNodes(ctx, kind, ca, true); err != nil {
		return GKEAccess{}, err
	}
	if err := g.waitAPIServer(ctx, kind, ""); err != nil {
		return GKEAccess{}, err
	}
	if err := g.setClusterInfoCA(ctx, kind, ca); err != nil {
		return GKEAccess{}, err
	}
	// GKE re-creates the nodes, so every pod starts again with the new
	// CA; locally the pods are re-created on the nodes they ran on.
	if _, err := r.Run(ctx, "kubectl", "--kubeconfig", g.kubeconfig(kind), "delete", "pods", "--all", "--all-namespaces", "--wait=false"); err != nil {
		return GKEAccess{}, fmt.Errorf("re-create the pods: %w", err)
	}
	a, err := g.exportAccess(ctx, kind)
	if err != nil {
		return a, err
	}
	if err := g.closePreviousEndpoint(ctx, kind); err != nil {
		return GKEAccess{}, err
	}
	return a, nil
}

// trustOnNodes writes bundle as every node's cluster CA; with reissue the
// kubelets' client certificates are issued again from the control plane's
// (new) CA and their kubeconfigs trust bundle.
func (g *GKE) trustOnNodes(ctx context.Context, kind, bundle string, reissue bool) error {
	r := g.runner()
	nodes, err := g.clusterNodes(ctx, kind)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if !reissue && n == kind+"-control-plane" {
			// The control plane's ca.crt is the new CA alone, the one the
			// controller manager signs with; its kubelet reads it again.
			if _, err := r.Run(ctx, "docker", "exec", n, "systemctl", "restart", "kubelet"); err != nil {
				return fmt.Errorf("restart the control plane's kubelet: %w", err)
			}
			continue
		}
		creds := ""
		if reissue {
			if creds, err = r.Run(ctx, "docker", "exec", kind+"-control-plane", "sh", "-c", kubeletCertScript, "sh", n); err != nil {
				return fmt.Errorf("issue node %s's kubelet certificate: %w", n, err)
			}
		}
		if _, err := r.Run(ctx, "docker", "exec", n, "sh", "-c", nodeTrustScript, "sh", bundle, creds); err != nil {
			return fmt.Errorf("make node %s trust the cluster CA: %w", n, err)
		}
	}
	return nil
}

// waitAPIServer waits for the API server to run with --client-ca-file
// naming file (any when "") and to answer, through the admin credentials.
func (g *GKE) waitAPIServer(ctx context.Context, kind, file string) error {
	r := g.runner()
	cp := kind + "-control-plane"
	deadline := time.Now().Add(4 * time.Minute)
	for {
		args, _ := r.Run(ctx, "docker", "exec", cp, "sh", "-c", apiServerArgs)
		ok := strings.TrimSpace(args) != ""
		if file != "" {
			ok = ok && strings.Contains(args, "--client-ca-file="+pki+"/"+file)
		} else {
			ok = ok && strings.Contains(args, "--client-ca-file="+pki+"/ca.crt")
		}
		if ok {
			if _, err := r.Run(ctx, "docker", "exec", cp, "kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "get", "--raw", "/readyz"); err == nil {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the API server did not come back with the rotated CA")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

var caDataRE = regexp.MustCompile(`certificate-authority-data: \S+`)

// setClusterInfoCA makes kube-public's cluster-info, which a joining node
// discovers the cluster with, name bundle as the cluster's CA.
func (g *GKE) setClusterInfoCA(ctx context.Context, kind, bundle string) error {
	r := g.runner()
	cp := kind + "-control-plane"
	raw, err := r.Run(ctx, "docker", "exec", cp, "kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "-n", "kube-public", "get", "configmap", "cluster-info", "-o", "json")
	if err != nil {
		return fmt.Errorf("read cluster-info: %w", err)
	}
	var cm map[string]any
	if err := json.Unmarshal([]byte(raw), &cm); err != nil {
		return err
	}
	data, _ := cm["data"].(map[string]any)
	kc, _ := data["kubeconfig"].(string)
	if kc == "" {
		return fmt.Errorf("cluster-info has no kubeconfig")
	}
	data["kubeconfig"] = caDataRE.ReplaceAllString(kc, "certificate-authority-data: "+base64.StdEncoding.EncodeToString([]byte(bundle)))
	// The JWS signatures sign the kubeconfig: the bootstrap signer signs
	// it again.
	for k := range data {
		if strings.HasPrefix(k, "jws-kubeconfig-") {
			delete(data, k)
		}
	}
	meta, _ := cm["metadata"].(map[string]any)
	cm["metadata"] = map[string]any{"name": meta["name"], "namespace": meta["namespace"]}
	b, err := json.Marshal(cm)
	if err != nil {
		return err
	}
	f := filepath.Join(g.Dir, kind, "cluster-info.json")
	if err := os.WriteFile(f, b, 0o600); err != nil {
		return err
	}
	defer os.Remove(f)
	if _, err := r.Run(ctx, "kubectl", "--kubeconfig", g.kubeconfig(kind), "replace", "-f", f); err != nil {
		return fmt.Errorf("write cluster-info: %w", err)
	}
	return nil
}

// exportAccess writes kind's admin kubeconfig, as kind exports it, with
// the cluster's current endpoint, and returns the cluster's access.
func (g *GKE) exportAccess(ctx context.Context, kind string) (GKEAccess, error) {
	r := g.runner()
	kc := g.kubeconfig(kind)
	if _, err := r.Run(ctx, "kind", "export", "kubeconfig", "--name", kind, "--kubeconfig", kc); err != nil {
		return GKEAccess{}, fmt.Errorf("export the kubeconfig: %w", err)
	}
	if port := g.endpointPorts(kind).serving(); port != defaultAPIPort {
		hp, err := g.hostPort(ctx, kind, port)
		if err != nil {
			return GKEAccess{}, err
		}
		if _, err := r.Run(ctx, "kubectl", "--kubeconfig", kc, "config", "set-cluster", "kind-"+kind, "--server=https://"+hp); err != nil {
			return GKEAccess{}, fmt.Errorf("point the kubeconfig at %s: %w", hp, err)
		}
	}
	out, err := r.Run(ctx, "kubectl", "--kubeconfig", kc, "config", "view", "--raw", "-o", "json")
	if err != nil {
		return GKEAccess{}, fmt.Errorf("read the kubeconfig: %w", err)
	}
	return parseAccess(out)
}

// emulationScript runs the control plane's components with
// --emulated-version=$1, or without the flag when $1 is empty.
const emulationScript = `set -e
cd /etc/kubernetes/manifests
for c in kube-apiserver kube-controller-manager kube-scheduler; do
  sed -i -e '/--emulated-version=/d' "$c.yaml"
  if [ -n "$1" ]; then
    sed -i -e "s/^\\( *\\)- $c\$/&\\n\\1- --emulated-version=$1/" "$c.yaml"
    grep -q -- "--emulated-version=$1" "$c.yaml" || { echo "$c.yaml has no $c command" >&2; exit 1; }
  fi
done
`

// SetEmulatedVersion runs the control plane with Kubernetes' compatibility
// version minor ("1.35"), the behaviour of that minor version on the newer
// binaries, or without one when minor is "", and waits for the API server
// to report it.
func (g *GKE) SetEmulatedVersion(ctx context.Context, kind, minor string) error {
	if !strings.HasPrefix(kind, OwnerPrefix) {
		return fmt.Errorf("%w: %q", ErrNotOwned, kind)
	}
	r := g.runner()
	cp := kind + "-control-plane"
	if _, err := r.Run(ctx, "docker", "exec", cp, "sh", "-c", emulationScript, "sh", minor); err != nil {
		return fmt.Errorf("set the emulated version: %w", err)
	}
	deadline := time.Now().Add(4 * time.Minute)
	for {
		args, _ := r.Run(ctx, "docker", "exec", cp, "sh", "-c", apiServerArgs)
		has := strings.Contains(args, "--emulated-version=")
		if strings.TrimSpace(args) != "" && has == (minor != "") {
			if _, err := r.Run(ctx, "kubectl", "--kubeconfig", g.kubeconfig(kind), "get", "--raw", "/readyz"); err == nil {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the API server did not come back with emulated version %q", minor)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// The control plane's address (#2100). GKE serves a rotating cluster on
// its old and a new IP address, and stops serving on the old one when the
// rotation completes (re-read 2026-11-21: how-to/ip-rotation). Locally an
// address is a host port: a GKE cluster's control plane publishes the API
// server's port (6443) and a spare one (6444) when it is made, and the
// two take turns. Inside the control plane's network namespace:
//   - the spare port serves only while it is in use, redirected to the API
//     server (an iptables REDIRECT);
//   - a port no longer in use refuses new connections from the host (from
//     the kind network's gateway, which Docker's published ports arrive
//     from), while the nodes and pods, which reach the API server through
//     the network directly, are not touched.
// A cluster made before the spare port existed keeps its one address.

const (
	defaultAPIPort = 6443
	spareAPIPort   = 6444
)

// endpointState is which container port serves the host, and which a
// rotation in progress serves too, kept in the cluster's directory.
type endpointState struct {
	Serving int `json:"serving"`
	Next    int `json:"next,omitempty"`
}

func (s endpointState) serving() int {
	if s.Next != 0 {
		return s.Next
	}
	if s.Serving == 0 {
		return defaultAPIPort
	}
	return s.Serving
}

func (g *GKE) endpointFile(kind string) string { return filepath.Join(g.Dir, kind, "endpoint.json") }

func (g *GKE) endpointPorts(kind string) endpointState {
	var st endpointState
	if b, err := os.ReadFile(g.endpointFile(kind)); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.Serving == 0 {
		st.Serving = defaultAPIPort
	}
	return st
}

func (g *GKE) saveEndpointPorts(kind string, st endpointState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(g.endpointFile(kind), b, 0o600)
}

// hostPort is the host address a container port of the control plane is
// published on.
func (g *GKE) hostPort(ctx context.Context, kind string, port int) (string, error) {
	out, err := g.runner().Run(ctx, "docker", "port", kind+"-control-plane", fmt.Sprintf("%d/tcp", port))
	if err != nil {
		return "", fmt.Errorf("read the control plane's published port %d: %w", port, err)
	}
	for _, l := range strings.Fields(out) {
		if strings.HasPrefix(l, "127.0.0.1:") {
			return l, nil
		}
	}
	return "", fmt.Errorf("the control plane's port %d is not published on 127.0.0.1", port)
}

// gateway is the kind network's IPv4 gateway, the source of the host's
// connections through published ports.
func (g *GKE) gateway(ctx context.Context) (string, error) {
	out, err := g.runner().Run(ctx, "docker", "network", "inspect", "kind", "-f", "{{range .IPAM.Config}}{{.Gateway}} {{end}}")
	if err != nil {
		return "", fmt.Errorf("read the kind network's gateway: %w", err)
	}
	for _, f := range strings.Fields(out) {
		if strings.Count(f, ".") == 3 {
			return f, nil
		}
	}
	return "", fmt.Errorf("the kind network has no IPv4 gateway")
}

// endpointRules adds (-A) or deletes (-D) the iptables rule that serves
// (open) or stops serving (closed) a port to the host.
func endpointRule(op string, port int, open bool, gw string) []string {
	if port == spareAPIPort && open {
		return []string{"iptables", "-t", "nat", op, "PREROUTING", "-p", "tcp", "--dport", "6444", "-j", "REDIRECT", "--to-ports", "6443"}
	}
	if port == defaultAPIPort && !open {
		return []string{"iptables", op, "INPUT", "-p", "tcp", "-s", gw, "--dport", "6443",
			"-m", "conntrack", "--ctstate", "NEW", "--ctorigdstport", "6443", "-j", "REJECT", "--reject-with", "tcp-reset"}
	}
	return nil
}

// setEndpoint opens or closes a port to the host.
func (g *GKE) setEndpoint(ctx context.Context, kind string, port int, open bool) error {
	gw, err := g.gateway(ctx)
	if err != nil {
		return err
	}
	cp := kind + "-control-plane"
	r := g.runner()
	if rule := endpointRule("-A", port, open, gw); rule != nil {
		check := endpointRule("-C", port, open, gw)
		if _, err := r.Run(ctx, "docker", append([]string{"exec", cp}, check...)...); err != nil {
			if _, err := r.Run(ctx, "docker", append([]string{"exec", cp}, rule...)...); err != nil {
				return fmt.Errorf("serve port %d: %w", port, err)
			}
		}
	}
	// The opposite rule goes: a port opened is no longer closed, and a
	// port closed no longer redirected.
	if rule := endpointRule("-D", port, !open, gw); rule != nil {
		check := endpointRule("-C", port, !open, gw)
		if _, err := r.Run(ctx, "docker", append([]string{"exec", cp}, check...)...); err == nil {
			if _, err := r.Run(ctx, "docker", append([]string{"exec", cp}, rule...)...); err != nil {
				return fmt.Errorf("change port %d: %w", port, err)
			}
		}
	}
	return nil
}

// openNextEndpoint serves the other port too, when the cluster has the
// spare one.
func (g *GKE) openNextEndpoint(ctx context.Context, kind string) error {
	if _, err := g.hostPort(ctx, kind, spareAPIPort); err != nil {
		return nil // made before the spare port: one address
	}
	st := g.endpointPorts(kind)
	next := spareAPIPort
	if st.Serving == spareAPIPort {
		next = defaultAPIPort
	}
	if err := g.setEndpoint(ctx, kind, next, true); err != nil {
		return err
	}
	st.Next = next
	return g.saveEndpointPorts(kind, st)
}

// closePreviousEndpoint stops serving the port the rotation moved from.
func (g *GKE) closePreviousEndpoint(ctx context.Context, kind string) error {
	st := g.endpointPorts(kind)
	if st.Next == 0 {
		return nil
	}
	if err := g.setEndpoint(ctx, kind, st.Serving, false); err != nil {
		return err
	}
	return g.saveEndpointPorts(kind, endpointState{Serving: st.Next})
}

// KeepEndpoint puts back the rules the cluster's endpoint state needs, in
// the control plane's network namespace: iptables rules live there, so a
// restarted control plane container (Docker or the machine restarting)
// loses them, and a rotated cluster would serve its old endpoint again,
// or no longer its new one. During a rotation both ports serve; after
// one, the port in use serves and the other refuses. A cluster on its
// first port with no rotation under way has no rules and nothing is run.
func (g *GKE) KeepEndpoint(ctx context.Context, kind string) error {
	if !strings.HasPrefix(kind, OwnerPrefix) {
		return fmt.Errorf("%w: %q", ErrNotOwned, kind)
	}
	st := g.endpointPorts(kind)
	if st.Serving == defaultAPIPort && st.Next == 0 {
		return nil
	}
	if _, err := g.hostPort(ctx, kind, spareAPIPort); err != nil {
		return nil // made before the spare port: one address
	}
	if err := g.setEndpoint(ctx, kind, st.Serving, true); err != nil {
		return err
	}
	if st.Next != 0 {
		return g.setEndpoint(ctx, kind, st.Next, true)
	}
	other := spareAPIPort
	if st.Serving == spareAPIPort {
		other = defaultAPIPort
	}
	return g.setEndpoint(ctx, kind, other, false)
}
