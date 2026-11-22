package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// Cloud Router's BGP speakers (#2140, internal/service/compute/
// bgpspeakers.go). Every few seconds the loop below:
//   - asks the compute API for the speakers its routers have (one per
//     subnetwork interface with BGP peers) and their gobgpd configuration;
//   - runs one pod for each, named for its configuration, so a change to
//     the router makes a new pod: the VM image's own vpc agent, which joins
//     the network's VXLAN segment at the interface's address, beside
//     GoBGP's gobgpd, reading the configuration from a Secret (it holds the
//     MD5 keys) — and deletes the pods and Secrets no speaker wants;
//   - puts the prefixes the speaker advertises in gobgpd's RIB (`gobgp
//     global rib add`), its export policy deciding which peer is sent
//     which, and takes the rest out;
//   - reads each session's state and what each peer advertised (`gobgp
//     neighbor -j`, `gobgp neighbor ADDR adj-in -j`) and gives them to the
//     compute API (SetBGPState), which getRouterStatus and the VMs' dynamic
//     routes come from.
//
// gobgpd and gobgp are GoBGP v3.37.0 (Apache-2.0), built with `go install`
// at that version (checked against sum.golang.org) in a pinned golang
// image the first time a router needs a speaker, into an image tagged by
// the Dockerfile's hash and loaded into the kind cluster; nothing of GoBGP
// is in CloudBurrow's binary (dependencies.json, "gobgp").

const bgpSpeakerLabel = "cloudburrow.dev/bgp-speaker"

// gobgpDockerfile builds GoBGP's daemon and CLI, statically, into an
// image of nothing else.
const gobgpDockerfile = `# GoBGP v3.37.0 (Apache-2.0, https://github.com/osrg/gobgp): Cloud Router's
# BGP speaker (#2140). Built by CloudBurrow at first use; see dependencies.json.
FROM golang:1.24-alpine@sha256:8bee1901f1e530bfb4a7850aa7a479d17ae3a18beb6e09064ed54cfd245b7191 AS build
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org
RUN go install -ldflags=-s github.com/osrg/gobgp/v3/cmd/gobgpd@v3.37.0 github.com/osrg/gobgp/v3/cmd/gobgp@v3.37.0
FROM scratch
COPY --from=build /go/bin/gobgpd /go/bin/gobgp /usr/bin/
ENTRYPOINT ["/usr/bin/gobgpd"]
`

// gobgpImageRef is the GoBGP image's tag: the Dockerfile's hash.
func gobgpImageRef() string {
	h := sha256.Sum256([]byte(gobgpDockerfile))
	return "cloudburrow-gobgp:" + hex.EncodeToString(h[:])[:12]
}

type routerSpeakers struct {
	svc *computeService
}

func (s *computeService) startRouterBGP(ctx context.Context) {
	b := &routerSpeakers{svc: s}
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if err := b.sync(ctx); err != nil && ctx.Err() == nil {
				fmt.Println("cloudburrow: Cloud Router BGP:", err)
			}
		}
	}()
}

// bgpPodName is the pod (and Secret) of speaker sp: its token and its
// configuration's hash.
func bgpPodName(sp compute.BGPSpeaker) string {
	h := sha256.Sum256([]byte(sp.Config + "\n" + sp.IP))
	return "cb-bgp-" + sp.Token[3:13] + "-" + hex.EncodeToString(h[:])[:8]
}

// bgpPod is a speaker pod as kubectl lists it.
type bgpPod struct {
	name, phase string
	ready       bool
}

func bgpPods(kubectlJSON string) map[string]bgpPod {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Phase             string `json:"phase"`
				ContainerStatuses []struct {
					Name  string `json:"name"`
					Ready bool   `json:"ready"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	out := map[string]bgpPod{}
	if json.Unmarshal([]byte(kubectlJSON), &list) != nil {
		return out
	}
	for _, it := range list.Items {
		p := bgpPod{name: it.Metadata.Name, phase: it.Status.Phase}
		for _, c := range it.Status.ContainerStatuses {
			if c.Name == "bgp" {
				p.ready = c.Ready
			}
		}
		out[p.name] = p
	}
	return out
}

func (b *routerSpeakers) sync(ctx context.Context) error {
	api, vms := b.svc.API(), b.svc.vms
	if api == nil || vms == nil {
		return nil
	}
	speakers := api.BGPSpeakers()
	out, err := vms.kubectl(ctx, "get", "pods", "-l", bgpSpeakerLabel, "-o", "json")
	if err != nil {
		return err
	}
	pods := bgpPods(out)
	want := map[string]compute.BGPSpeaker{}
	for _, sp := range speakers {
		want[bgpPodName(sp)] = sp
	}
	var errs []string
	for name, sp := range want {
		if _, ok := pods[name]; ok {
			continue
		}
		if err := b.start(ctx, name, sp); err != nil {
			errs = append(errs, err.Error())
		}
	}
	for name := range pods {
		if _, ok := want[name]; !ok {
			_, _ = vms.kubectl(ctx, "delete", "pod", name, "--wait=false", "--ignore-not-found")
		}
	}
	if secrets, err := vms.kubectl(ctx, "get", "secrets", "-l", bgpSpeakerLabel, "-o", "name"); err == nil {
		for _, s := range strings.Fields(secrets) {
			if _, ok := want[strings.TrimPrefix(s, "secret/")]; !ok {
				_, _ = vms.kubectl(ctx, "delete", s, "--wait=false", "--ignore-not-found")
			}
		}
	}
	for name, sp := range want {
		if p, ok := pods[name]; !ok || p.phase != "Running" || !p.ready {
			continue
		}
		st, err := b.speak(ctx, name, sp)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		api.SetBGPState(sp.Key, st)
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// start makes speaker sp's Secret and pod.
func (b *routerSpeakers) start(ctx context.Context, name string, sp compute.BGPSpeaker) error {
	vms := b.svc.vms
	img, ok := gatewayImage()
	if !ok {
		return fmt.Errorf("no Debian image to run Cloud Router's speakers")
	}
	vmRef, err := vms.ensureImage(ctx, img)
	if err != nil {
		return err
	}
	bgpRef, err := ensureGoBGPImage(ctx, vms)
	if err != nil {
		return err
	}
	if err := vms.installer.Apply(ctx, bgpSecretManifest(vms.ns(), name, sp)); err != nil {
		return err
	}
	guest := ""
	if vms.guestBase != nil {
		if base := vms.guestBase(); base != "" {
			guest = base + compute.GuestPrefix + sp.Token
		}
	}
	return vms.installer.Apply(ctx, bgpPodManifest(vms.ns(), name, sp, vmRef, bgpRef, guest))
}

func bgpSecretManifest(ns, name string, sp compute.BGPSpeaker) string {
	m := map[string]any{"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": name, "namespace": ns,
			"labels": map[string]string{"cloudburrow.dev/owned": "true", bgpSpeakerLabel: sp.Token}},
		"stringData": map[string]string{"gobgpd.toml": sp.Config}}
	raw, _ := json.Marshal(m)
	return string(raw)
}

func bgpPodManifest(ns, name string, sp compute.BGPSpeaker, vmRef, bgpRef, guest string) string {
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": ns,
			"labels":      map[string]string{"cloudburrow.dev/owned": "true", bgpSpeakerLabel: sp.Token},
			"annotations": map[string]string{"cloudburrow.dev/bgp-speaker-key": sp.Key}},
		"spec": map[string]any{"restartPolicy": "Always", "terminationGracePeriodSeconds": 1,
			"automountServiceAccountToken": false, "enableServiceLinks": false,
			"containers": []any{
				map[string]any{"name": "vpc", "image": vmRef, "imagePullPolicy": "Never",
					"command":         []string{"/usr/lib/cloudburrow/vpc"},
					"securityContext": map[string]any{"privileged": true},
					"env": []any{map[string]string{"name": "CLOUDBURROW_GUEST_METADATA", "value": guest},
						map[string]string{"name": "CLOUDBURROW_INTERNAL_IP", "value": sp.IP}},
					"resources": map[string]any{"requests": map[string]string{"cpu": "10m", "memory": "16Mi"}}},
				map[string]any{"name": "bgp", "image": bgpRef, "imagePullPolicy": "Never",
					"args":            []string{"-f", "/etc/gobgp/gobgpd.toml", "-t", "toml", "--api-hosts", "127.0.0.1:50051"},
					"securityContext": map[string]any{"capabilities": map[string]any{"add": []string{"NET_ADMIN", "NET_BIND_SERVICE"}}},
					"volumeMounts":    []any{map[string]any{"name": "config", "mountPath": "/etc/gobgp", "readOnly": true}},
					"readinessProbe": map[string]any{"exec": map[string]any{"command": []string{"/usr/bin/gobgp", "global"}},
						"periodSeconds": 2},
					"resources": map[string]any{"requests": map[string]string{"cpu": "10m", "memory": "32Mi"}}}},
			"volumes": []any{map[string]any{"name": "config", "secret": map[string]any{"secretName": name}}}}}
	raw, _ := json.Marshal(pod)
	return string(raw)
}

// ensureGoBGPImage builds the GoBGP image when this machine has not got
// it, and loads it into the cluster.
func ensureGoBGPImage(ctx context.Context, b *computeVMs) (string, error) {
	ref := gobgpImageRef()
	b.build.Lock()
	defer b.build.Unlock()
	if b.built[ref] {
		return ref, nil
	}
	if _, err := vmDocker(ctx, "image", "inspect", "--format", "{{.Id}}", ref); err != nil {
		dir, err := os.MkdirTemp("", "cloudburrow-gobgp-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(dir)
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(gobgpDockerfile), 0o644); err != nil {
			return "", err
		}
		if _, err := vmDocker(ctx, "build", "--platform", "linux/"+runtime.GOARCH,
			"--label", "cloudburrow.dev/owned=true", "--label", "cloudburrow.dev/gobgp=v3.37.0", "-t", ref, dir); err != nil {
			return "", fmt.Errorf("build the GoBGP image: %w", err)
		}
	}
	if out, err := exec.CommandContext(ctx, "kind", "load", "docker-image", ref, "--name", b.cfg.ClusterName()).CombinedOutput(); err != nil {
		return "", fmt.Errorf("load the GoBGP image into the cluster: %w: %s", err, vmTail(string(out), 3))
	}
	b.built[ref] = true
	return ref, nil
}

// gobgp runs the GoBGP CLI in pod's gobgpd container.
func (b *routerSpeakers) gobgp(ctx context.Context, pod string, args ...string) (string, error) {
	return b.svc.vms.kubectl(ctx, append([]string{"exec", pod, "-c", "bgp", "--", "/usr/bin/gobgp"}, args...)...)
}

// speak puts sp's prefixes in its RIB and reads its sessions.
func (b *routerSpeakers) speak(ctx context.Context, pod string, sp compute.BGPSpeaker) (compute.BGPState, error) {
	rib, err := b.gobgp(ctx, pod, "global", "rib", "-a", "ipv4", "-j")
	if err != nil {
		return compute.BGPState{}, fmt.Errorf("read %s's RIB: %w", pod, err)
	}
	have := localPrefixes(rib)
	for _, p := range sp.Advertise {
		if !have[p] {
			if _, err := b.gobgp(ctx, pod, "global", "rib", "add", "-a", "ipv4", p, "nexthop", sp.IP); err != nil {
				return compute.BGPState{}, fmt.Errorf("advertise %s from %s: %w", p, pod, err)
			}
		}
		delete(have, p)
	}
	for p := range have {
		_, _ = b.gobgp(ctx, pod, "global", "rib", "del", "-a", "ipv4", p)
	}
	nj, err := b.gobgp(ctx, pod, "neighbor", "-j")
	if err != nil {
		return compute.BGPState{}, fmt.Errorf("read %s's sessions: %w", pod, err)
	}
	ns := parseNeighbors(nj)
	var logs string
	for i, n := range ns {
		if n.State != 6 {
			if strings.Contains(sp.Config, "auth-password") {
				if logs == "" {
					logs, _ = b.svc.vms.kubectl(ctx, "logs", pod, "-c", "bgp", "--tail", "200")
				}
				ns[i].MD5Failed = md5Failed(logs, n.Address)
			}
			continue
		}
		adj, err := b.gobgp(ctx, pod, "neighbor", n.Address, "adj-in", "-a", "ipv4", "-j")
		if err != nil {
			return compute.BGPState{}, fmt.Errorf("read what %s advertised: %w", n.Address, err)
		}
		ns[i].Learned = parseRoutes(adj)
	}
	return compute.BGPState{Neighbors: ns}, nil
}

// md5Failed is whether gobgpd's log says it could not set address's MD5
// key.
func md5Failed(logs, address string) bool {
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, `"failed to set md5"`) && strings.Contains(l, `"Key":"`+address+`"`) {
			return true
		}
	}
	return false
}

// gobgpPath is one path of `gobgp ... rib -j` or `adj-in -j`.
type gobgpPath struct {
	Neighbor string `json:"neighbor-ip"`
	Attrs    []struct {
		Type    int    `json:"type"`
		NextHop string `json:"nexthop"`
		Metric  *int64 `json:"metric"`
		ASPaths []struct {
			ASNs []uint32 `json:"asns"`
		} `json:"as_paths"`
	} `json:"attrs"`
}

// localPrefixes are the prefixes of a RIB that gobgpd was given (no peer
// advertised them).
func localPrefixes(ribJSON string) map[string]bool {
	var rib map[string][]gobgpPath
	_ = json.Unmarshal([]byte(ribJSON), &rib)
	out := map[string]bool{}
	for p, paths := range rib {
		for _, x := range paths {
			if x.Neighbor == "" || x.Neighbor == "<nil>" {
				out[p] = true
			}
		}
	}
	return out
}

// parseRoutes are the routes of `gobgp neighbor ADDR adj-in -j`.
func parseRoutes(adjJSON string) []compute.BGPRoute {
	var rib map[string][]gobgpPath
	_ = json.Unmarshal([]byte(adjJSON), &rib)
	var out []compute.BGPRoute
	for p, paths := range rib {
		for _, x := range paths {
			r := compute.BGPRoute{Prefix: p, MED: -1}
			for _, a := range x.Attrs {
				switch a.Type {
				case 2:
					for _, seg := range a.ASPaths {
						r.ASPath = append(r.ASPath, seg.ASNs...)
					}
				case 3:
					r.NextHop = a.NextHop
				case 4:
					if a.Metric != nil {
						r.MED = *a.Metric
					}
				}
			}
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Prefix != out[j].Prefix {
			return out[i].Prefix < out[j].Prefix
		}
		return out[i].NextHop < out[j].NextHop
	})
	return out
}

// parseNeighbors are the sessions of `gobgp neighbor -j`.
func parseNeighbors(neighborJSON string) []compute.BGPNeighbor {
	var list []struct {
		State struct {
			Address string `json:"neighbor_address"`
			Session int    `json:"session_state"`
		} `json:"state"`
		Conf struct {
			Address string `json:"neighbor_address"`
		} `json:"conf"`
		Timers struct {
			State struct {
				Uptime struct {
					Seconds int64 `json:"seconds"`
				} `json:"uptime"`
			} `json:"state"`
		} `json:"timers"`
	}
	_ = json.Unmarshal([]byte(neighborJSON), &list)
	var out []compute.BGPNeighbor
	for _, n := range list {
		a := n.Conf.Address
		if a == "" {
			a = n.State.Address
		}
		nb := compute.BGPNeighbor{Address: a, State: n.State.Session}
		if nb.State == 0 {
			nb.State = 1
		}
		if nb.State == 6 && n.Timers.State.Uptime.Seconds > 0 {
			nb.Up = time.Unix(n.Timers.State.Uptime.Seconds, 0)
		}
		out = append(out, nb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}
