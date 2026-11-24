package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// gobgp v3.37.0's output, as `gobgp neighbor -j`, `gobgp neighbor ADDR
// adj-in -a ipv4 -j` and `gobgp global rib -a ipv4 -j` printed it for an
// established session (trimmed of capabilities).
const (
	gobgpNeighborJSON = `[{"conf":{"local_asn":64512,"neighbor_address":"172.29.77.11","peer_asn":65001,"type":1},"state":{"local_asn":64512,"neighbor_address":"172.29.77.11","peer_asn":65001,"type":1,"session_state":6,"router_id":"172.29.77.11"},"timers":{"config":{"connect_retry":120,"hold_time":60,"keepalive_interval":20},"state":{"keepalive_interval":20,"negotiated_hold_time":60,"uptime":{"seconds":1795364266},"downtime":{"seconds":1795364266}}}},{"conf":{"neighbor_address":"172.29.77.12","peer_asn":65002},"state":{"session_state":3},"timers":{"state":{}}}]`
	gobgpAdjInJSON    = `{"192.168.77.0/24":[{"nlri":{"prefix":"192.168.77.0/24"},"age":1795364270,"best":false,"attrs":[{"type":1,"value":2},{"type":2,"as_paths":[{"segment_type":2,"num":1,"asns":[65001]}]},{"type":3,"nexthop":"172.29.77.11"},{"type":4,"metric":50}],"stale":false,"source-id":"172.29.77.11","neighbor-ip":"172.29.77.11"}]}`
	gobgpRIBJSON      = `{"10.128.0.0/20":[{"nlri":{"prefix":"10.128.0.0/20"},"age":1795364270,"best":true,"attrs":[{"type":1,"value":2},{"type":3,"nexthop":"172.29.77.10"}],"stale":false}],"192.168.77.0/24":[{"nlri":{"prefix":"192.168.77.0/24"},"best":true,"attrs":[{"type":3,"nexthop":"172.29.77.11"}],"source-id":"172.29.77.11","neighbor-ip":"172.29.77.11"}]}`
)

func TestGoBGPOutputParsed(t *testing.T) {
	ns := parseNeighbors(gobgpNeighborJSON)
	if len(ns) != 2 || ns[0].Address != "172.29.77.11" || ns[0].State != 6 || ns[0].Up.Unix() != 1795364266 ||
		ns[1].State != 3 || !ns[1].Up.IsZero() {
		t.Fatalf("neighbors: %+v", ns)
	}
	rs := parseRoutes(gobgpAdjInJSON)
	if len(rs) != 1 || rs[0].Prefix != "192.168.77.0/24" || rs[0].NextHop != "172.29.77.11" || rs[0].MED != 50 ||
		len(rs[0].ASPath) != 1 || rs[0].ASPath[0] != 65001 {
		t.Fatalf("routes: %+v", rs)
	}
	if rs := parseRoutes(`{"10.0.0.0/8":[{"attrs":[{"type":3,"nexthop":"10.1.1.1"}]}]}`); rs[0].MED != -1 {
		t.Fatalf("no MED: %+v", rs)
	}
	if !md5Failed(`{"Err":"protocol not available","Key":"172.29.77.11","Topic":"Peer","level":"warning","msg":"failed to set md5"}`, "172.29.77.11") {
		t.Fatal("an MD5 failure not seen")
	}
	if lp := localPrefixes(gobgpRIBJSON); len(lp) != 1 || !lp["10.128.0.0/20"] {
		t.Fatalf("local prefixes: %v", lp)
	}
}

// TestBGPSpeakerPod: the pod joins the segment with the VM image's vpc
// agent and runs gobgpd beside it from the Secret, named for the
// configuration so a change makes a new pod.
func TestBGPSpeakerPod(t *testing.T) {
	sp := compute.BGPSpeaker{Key: "projects/p/regions/r/routers/r1/if0", Token: "bgp0123456789abcdef01234567", IP: "10.128.0.250", Config: "[global.config]\n"}
	name := bgpPodName(sp)
	if !strings.HasPrefix(name, "cb-bgp-0123456789-") || len(name) > 63 {
		t.Fatalf("pod name %q", name)
	}
	sp2 := sp
	sp2.Config += "x"
	if bgpPodName(sp2) == name {
		t.Fatal("a new configuration keeps the pod")
	}
	var pod struct {
		Spec struct {
			Containers []struct {
				Name, Image string
				Command     []string
				Args        []string
				Env         []map[string]string
			}
			Volumes []struct {
				Secret struct {
					SecretName string `json:"secretName"`
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(bgpPodManifest("ns", name, sp, "vm:1", gobgpImageRef(), "http://h/cloudburrow/guest/"+sp.Token)), &pod); err != nil {
		t.Fatal(err)
	}
	c := pod.Spec.Containers
	if len(c) != 2 || c[0].Command[0] != "/usr/lib/cloudburrow/vpc" || c[0].Env[1]["value"] != sp.IP ||
		c[1].Image != gobgpImageRef() || strings.Join(c[1].Args, " ") != "-f /etc/gobgp/gobgpd.toml -t toml --api-hosts 127.0.0.1:50051" ||
		pod.Spec.Volumes[0].Secret.SecretName != name {
		t.Fatalf("pod: %+v", pod.Spec)
	}
	if !strings.Contains(bgpSecretManifest("ns", name, sp), `"gobgpd.toml":"[global.config]\n"`) {
		t.Fatal("secret lacks the configuration")
	}
	if !strings.Contains(gobgpDockerfile, "@sha256:") || !strings.Contains(gobgpDockerfile, "gobgpd@v3.37.0") {
		t.Fatal("the GoBGP build is not pinned")
	}
}
