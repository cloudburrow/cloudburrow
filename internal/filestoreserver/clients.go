package filestoreserver

// The instances' clients (#2192): the kind nodes of the CloudBurrow
// instance's cluster, whose pods are Compute Engine's VMs, and of its
// Kubernetes Engine clusters (Cluster-gke-HASH).
//
// Addresses. An instance serves at an address of its VPC network, in its
// reserved range; its server's container has an address on the Docker
// network. Each client node sends the instance's address on to the
// server's: a DNAT rule in the node's network namespace, in a chain of its
// own jumped to from PREROUTING (the pods' and VMs' connections) and
// OUTPUT (the kubelet's NFS mounts). The rules are kept in step every few
// seconds, so a node made after the instance (a new cluster, a new pool)
// gets them before its first pod mounts.
//
// Deleted instances. On Google, deleting an instance leaves the clients
// that still mount its share with stale file handles, while the VMs and
// clusters that mount it can still be deleted. A Linux NFS client whose
// server has gone instead waits on it, and a kind node waiting on it
// cannot be removed. So when a client still mounts a deleted instance's
// share, the instance's address is answered by a tombstone: an NFS server
// with no exports, its files gone, at the same address, which answers the
// clients' handles NFS3ERR_STALE or NFS4ERR_STALE (ESTALE to programs)
// and lets them unmount. It is removed once no client mounts the address.

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/filestore"
)

// chain is the nodes' DNAT chain.
const chain = "CLOUDBURROW-FILESTORE"

// tombLabel marks a tombstone and names the address it answers.
const tombLabel = Label + ".tombstone"

// loopEvery is how often the nodes' rules and the tombstones are checked.
const loopEvery = 4 * time.Second

// nodes are the running kind nodes of the instance's clusters.
func (d *Docker) nodes(ctx context.Context) ([]string, error) {
	if d.Cluster == "" {
		return nil, nil
	}
	out, err := d.docker(ctx, "ps", "--filter", "label=io.x-k8s.kind.cluster", "--format", `{{.Names}} {{.Label "io.x-k8s.kind.cluster"}}`)
	if err != nil {
		return nil, err
	}
	var nodes []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && ownCluster(d.Cluster, f[1]) {
			nodes = append(nodes, f[0])
		}
	}
	sort.Strings(nodes)
	return nodes, nil
}

// ownCluster is whether kind cluster c is the instance's (cluster) or one
// of its Kubernetes Engine clusters'.
func ownCluster(cluster, c string) bool {
	if c == cluster {
		return true
	}
	rest, ok := strings.CutPrefix(c, cluster+"-gke-")
	if !ok || len(rest) != 8 {
		return false
	}
	for _, r := range rest {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// Rules is the iptables-restore input that sends each address on to its
// server: address -> server address.
func Rules(routes map[string]string) string {
	addrs := make([]string, 0, len(routes))
	for a := range routes {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	var b strings.Builder
	b.WriteString("*nat\n:" + chain + " - [0:0]\n")
	for _, a := range addrs {
		fmt.Fprintf(&b, "-A %s -d %s/32 -j DNAT --to-destination %s\n", chain, a, routes[a])
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

// applyScript loads the rules (stdin) into the node's chain, which the
// declaration flushes first, and jumps to it from PREROUTING and OUTPUT.
const applyScript = `set -e
iptables-restore --noflush
for c in PREROUTING OUTPUT; do
  iptables -t nat -C "$c" -j ` + chain + ` 2>/dev/null || iptables -t nat -I "$c" 1 -j ` + chain + `
done`

// routesLocked is the address -> server address map the nodes take.
func (d *Docker) routesLocked() map[string]string {
	out := map[string]string{}
	for a, r := range d.addrs {
		out[a] = r
	}
	return out
}

// route makes address reach server address ip (address "" is none).
func (d *Docker) route(address, ip string) {
	if address == "" {
		return
	}
	d.rmu.Lock()
	if d.addrs == nil {
		d.addrs = map[string]string{}
	}
	d.addrs[address] = ip
	d.rmu.Unlock()
	d.startLoop()
	d.kick()
}

func (d *Docker) unroute(address string) {
	d.rmu.Lock()
	delete(d.addrs, address)
	d.rmu.Unlock()
	d.kick()
}

// kick asks the loop to sync the nodes now.
func (d *Docker) kick() {
	d.rmu.Lock()
	k := d.wake
	d.rmu.Unlock()
	if k == nil {
		return
	}
	select {
	case k <- struct{}{}:
	default:
	}
}

func (d *Docker) startLoop() {
	d.rmu.Lock()
	defer d.rmu.Unlock()
	if d.stopLoop != nil || d.Cluster == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.stopLoop = cancel
	d.wake = make(chan struct{}, 1)
	d.loopDone = make(chan struct{})
	go d.loop(ctx, d.wake, d.loopDone)
}

// endLoop stops the loop and waits for it.
func (d *Docker) endLoop() {
	d.rmu.Lock()
	stop, done := d.stopLoop, d.loopDone
	d.stopLoop, d.wake, d.loopDone = nil, nil, nil
	d.rmu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
}

func (d *Docker) loop(ctx context.Context, wake <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	applied := map[string]string{}
	appliedAt := map[string]time.Time{}
	t := time.NewTicker(loopEvery)
	defer t.Stop()
	for {
		d.syncNodes(ctx, applied, appliedAt)
		d.sweepTombstones(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-wake:
		}
	}
}

// syncNodes loads the rules into every node whose rules differ from them,
// and into each again every minute, in case it restarted.
func (d *Docker) syncNodes(ctx context.Context, applied map[string]string, appliedAt map[string]time.Time) {
	nodes, err := d.nodes(ctx)
	if err != nil {
		return
	}
	d.rmu.Lock()
	rules := Rules(d.routesLocked())
	d.rmu.Unlock()
	seen := map[string]bool{}
	for _, n := range nodes {
		seen[n] = true
		if applied[n] == rules && time.Since(appliedAt[n]) < time.Minute {
			continue
		}
		if err := d.runner().Run(ctx, strings.NewReader(rules), io.Discard, "docker", "exec", "-i", n, "sh", "-c", applyScript); err != nil {
			d.logf("filestore: node %s: %v", n, err)
			continue
		}
		applied[n], appliedAt[n] = rules, time.Now()
	}
	for n := range applied {
		if !seen[n] {
			delete(applied, n)
			delete(appliedAt, n)
		}
	}
}

// Synced is whether every node has taken the current rules, for tests.
func (d *Docker) Synced(ctx context.Context, node string) (bool, error) {
	var out strings.Builder
	if err := d.runner().Run(ctx, nil, &out, "docker", "exec", node, "iptables", "-t", "nat", "-S", chain); err != nil {
		return false, err
	}
	d.rmu.Lock()
	routes := d.routesLocked()
	d.rmu.Unlock()
	for a, ip := range routes {
		if !strings.Contains(out.String(), "-d "+a+"/32 -j DNAT --to-destination "+ip) {
			return false, nil
		}
	}
	return true, nil
}

// mountedScript prints the first process whose mounts hold a share of
// address $1 (or of the server's own address, $2).
const mountedScript = `grep -l -F -e " $1:/" -e " $2:/" /proc/[0-9]*/mountinfo 2>/dev/null | head -n 1`

// mounted is whether a client still mounts a share of address (or ip).
func (d *Docker) mounted(ctx context.Context, address, ip string) (bool, error) {
	nodes, err := d.nodes(ctx)
	if err != nil {
		return false, err
	}
	if address == "" {
		address = ip
	}
	for _, n := range nodes {
		out, err := d.docker(ctx, "exec", n, "sh", "-c", mountedScript, "sh", address, ip)
		if err != nil {
			if stuckNode(err) {
				// A node killed while its processes wait on an NFS
				// server cannot be read, and goes only once the server
				// answers them: so it may still mount address.
				return true, nil
			}
			// A node going away mounts nothing any more.
			continue
		}
		if strings.TrimSpace(out) != "" {
			return true, nil
		}
	}
	return false, nil
}

// stuckNode is whether err, a docker exec's into a node Docker lists as
// running, says the node's processes are gone but the node is not: its
// init was killed, and a process blocked in NFS I/O keeps it, so the exec
// cannot join it ("exit status 127", runc's setns failure). Docker cannot
// remove such a node ("did not receive an exit event") until its NFS
// server answers.
func stuckNode(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "exit status 127") || strings.Contains(msg, "error executing setns process")
}

func (d *Docker) tombstone(key string) string { return d.container(key) + "-deleted" }

// bury answers address, at server address ip, with a tombstone.
func (d *Docker) bury(ctx context.Context, key, address, ip string) error {
	img, err := d.image(ctx)
	if err != nil {
		return err
	}
	conf := Config(filestore.Spec{NFSv41: true})
	_, err = d.docker(ctx, "run", "-d", "--name", d.tombstone(key), "--hostname", "filestore-"+key,
		"--label", d.label(), "--label", "cloudburrow.dev/owned=true", "--label", tombLabel+"="+address,
		"--privileged", "--network", Network, "--ip", ip,
		"--sysctl", "net.ipv6.conf.all.disable_ipv6=1",
		"-e", "GANESHA_CONFIG="+base64.StdEncoding.EncodeToString([]byte(conf)), img)
	if err != nil {
		return fmt.Errorf("start the deleted instance's tombstone: %w", err)
	}
	if err := d.waitReady(ctx, d.tombstone(key)); err != nil {
		return err
	}
	d.route(address, ip)
	return nil
}

// sweepTombstones removes each tombstone no client mounts any more, or
// whose address a new instance took.
func (d *Docker) sweepTombstones(ctx context.Context) {
	out, err := d.docker(ctx, "ps", "-a", "--filter", "label="+d.label(), "--filter", "label="+tombLabel,
		"--format", `{{.Names}} {{.Label "`+tombLabel+`"}}`)
	if err != nil {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		name, address := f[0], f[1]
		ip, err := d.docker(ctx, "container", "inspect", "--format", `{{with index .NetworkSettings.Networks "kind"}}{{with .IPAMConfig}}{{.IPv4Address}}{{end}}{{end}}`, name)
		if err != nil {
			continue
		}
		d.rmu.Lock()
		taken := d.addrs[address] != "" && d.addrs[address] != ip
		d.rmu.Unlock()
		if !taken {
			if busy, err := d.mounted(ctx, address, ip); err != nil || busy {
				continue
			}
		}
		if _, err := d.docker(ctx, "rm", "-f", "-v", name); err != nil {
			continue
		}
		d.logf("filestore: removed the tombstone of %s: no client mounts it", address)
		if !taken {
			d.unroute(address)
		}
	}
}

// Tombstones are the addresses tombstones answer, for tests.
func (d *Docker) Tombstones(ctx context.Context) ([]string, error) {
	out, err := d.docker(ctx, "ps", "-a", "--filter", "label="+d.label(), "--filter", "label="+tombLabel,
		"--format", `{{.Label "`+tombLabel+`"}}`)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}
