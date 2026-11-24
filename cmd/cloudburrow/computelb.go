package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/egress"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// computeLB is what Cloud Load Balancing's data plane (#2153,
// internal/service/compute/lbplane.go) sends traffic to on this machine:
// an instance's port through a Service of its pod and a tunnel to it, as
// its SSH port is reached; the Cloud Run ingress; and the local APIs.
type computeLB struct {
	cfg config.Config
	vms *computeVMs
}

// lbTunnelKey is one instance port's tunnel.
func lbTunnelKey(vm compute.VMSpec, port int) string { return vm.Key + "|" + strconv.Itoa(port) }

// lbServiceName is the Service of an instance's port: a DNS-1035 label of
// at most 63 characters.
func lbServiceName(vm compute.VMSpec, port int) string {
	n := vm.Key + "-p" + strconv.Itoa(port)
	if len(n) > 63 {
		h := sha1.Sum([]byte(vm.Key))
		n = "vm-" + hex.EncodeToString(h[:12]) + "-p" + strconv.Itoa(port)
	}
	return n
}

// InstanceAddr starts (once) the tunnel to the instance's port and answers
// its host address once it is up; until then it answers an error, so a
// probe fails and a request goes to another instance, rather than either
// waiting for the tunnel.
func (l *computeLB) InstanceAddr(_ context.Context, vm compute.VMSpec, port int) (string, error) {
	b := l.vms
	key := lbTunnelKey(vm, port)
	b.mu.Lock()
	f := b.lbTunnels[key]
	if f == nil {
		name := lbServiceName(vm, port)
		f = netfwd.New(netfwd.Target{Name: name, Namespace: b.ns(), ServicePort: port, Label: "lb-" + name},
			l.cfg.KubeconfigPath(), l.cfg.BindAddress)
		b.lbTunnels[key] = f
		go func() {
			labels := map[string]string{k8s.OwnedLabel: k8s.OwnedValue, k8s.ServiceLabel: "compute", "cloudburrow.dev/lb-vm": vm.Key}
			svc := map[string]any{"apiVersion": "v1", "kind": "Service",
				"metadata": map[string]any{"name": name, "namespace": b.ns(), "labels": labels},
				"spec": map[string]any{"selector": map[string]string{"cloudburrow.dev/vm": vm.Key},
					"ports": []any{map[string]any{"name": "lb", "port": port, "targetPort": port}}}}
			raw, _ := json.Marshal(svc)
			if err := b.installer.Apply(b.bg, string(raw)); err != nil {
				b.dropLBTunnel(key, f)
				return
			}
			if err := f.Start(b.bg); err != nil {
				b.dropLBTunnel(key, f)
			}
		}()
	}
	b.mu.Unlock()
	if a := f.HostAddr(); a != "" && f.Running() {
		return a, nil
	}
	return "", fmt.Errorf("the tunnel to %s's port %d is not up yet", vm.Name, port)
}

// dropLBTunnel forgets a tunnel that failed, so the next request starts
// another.
func (b *computeVMs) dropLBTunnel(key string, f *netfwd.Forwarder) {
	b.mu.Lock()
	if b.lbTunnels[key] == f {
		delete(b.lbTunnels, key)
	}
	b.mu.Unlock()
	_ = f.Stop(context.Background())
}

// stopLBTunnels stops an instance's load-balancing tunnels and deletes
// their Services, as the instance stops.
func (b *computeVMs) stopLBTunnels(ctx context.Context, vm compute.VMSpec) {
	b.mu.Lock()
	var fs []*netfwd.Forwarder
	for k, f := range b.lbTunnels {
		if len(k) > len(vm.Key) && k[:len(vm.Key)+1] == vm.Key+"|" {
			fs = append(fs, f)
			delete(b.lbTunnels, k)
		}
	}
	b.mu.Unlock()
	for _, f := range fs {
		_ = f.Stop(context.Background())
	}
	if len(fs) > 0 {
		_, _ = b.kubectl(ctx, "delete", "service", "-l", "cloudburrow.dev/lb-vm="+vm.Key, "--ignore-not-found")
	}
}

// Ingress is the Knative gateway's host address, "" without Cloud Run.
func (l *computeLB) Ingress() string { return workflowsIngress(l.cfg) }

// LocalAPI is a local API's base URL, from up's bound addresses.
func (l *computeLB) LocalAPI(api string) string {
	if f := egress.Local(); f != nil {
		return f(api)
	}
	return ""
}

// udpRelayScript relays one UDP flow inside an instance (#2160): datagrams
// framed by a 2-byte big-endian length on stdin go to 127.0.0.1:PORT, and
// each reply comes back framed on stdout. Perl and IO::Socket are in
// perl-base, which every Debian and Ubuntu image has; kubectl's exec
// stream carries bytes only, so the framing keeps datagram boundaries.
const udpRelayScript = `use IO::Socket::INET; use IO::Select; binmode STDIN; binmode STDOUT; $|=1;
my $s=IO::Socket::INET->new(PeerAddr=>"127.0.0.1",PeerPort=>$ARGV[0],Proto=>"udp") or die "socket: $!";
my $sel=IO::Select->new(\*STDIN,$s);
while(my @r=$sel->can_read){for my $h(@r){
 if($h==$s){my $d; defined($s->recv($d,65535)) or next; syswrite(STDOUT,pack("n",length $d).$d);}
 else{my $l; sysread(STDIN,$l,2)==2 or exit 0; my $n=unpack("n",$l); my $d="";
  while(length($d)<$n){my $c; sysread(STDIN,$c,$n-length($d)) or exit 0; $d.=$c} $s->send($d);}}}`

// InstanceUDP is a datagram channel to the running instance's UDP port,
// through kubectl exec into its pod: Kubernetes' port forwarding carries
// TCP only.
func (l *computeLB) InstanceUDP(ctx context.Context, vm compute.VMSpec, port int) (compute.DatagramConn, error) {
	bg, cancel := context.WithCancel(l.vms.bg)
	cmd := exec.CommandContext(bg, "kubectl", "--kubeconfig", l.cfg.KubeconfigPath(), "-n", l.vms.ns(), "exec", "-i", vm.Key, "-c", "vm",
		"--", "perl", "-e", udpRelayScript, strconv.Itoa(port))
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	go func() { _ = cmd.Wait() }()
	return &execDatagrams{in: in, out: bufio.NewReader(out), cancel: cancel}, nil
}

// execDatagrams is a framed datagram stream over an exec's stdin and
// stdout.
type execDatagrams struct {
	mu     sync.Mutex
	in     io.WriteCloser
	out    *bufio.Reader
	cancel context.CancelFunc
}

func (d *execDatagrams) Send(b []byte) error {
	if len(b) > 65535 {
		return fmt.Errorf("datagram of %d bytes", len(b))
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	hdr := []byte{byte(len(b) >> 8), byte(len(b))}
	if _, err := d.in.Write(append(hdr, b...)); err != nil {
		return err
	}
	return nil
}

func (d *execDatagrams) Recv() ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(d.out, hdr[:]); err != nil {
		return nil, err
	}
	b := make([]byte, int(hdr[0])<<8|int(hdr[1]))
	_, err := io.ReadFull(d.out, b)
	return b, err
}

func (d *execDatagrams) Close() error {
	_ = d.in.Close()
	d.cancel()
	return nil
}
