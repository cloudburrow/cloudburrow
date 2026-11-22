package gke

import (
	"context"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// kubeletFake is upFake recording the kubelet settings each pool's nodes
// join with, and the events at the time they were recorded.
type kubeletFake struct {
	*upFake
	mu       sync.Mutex
	settings map[string]map[string]any
}

func (f *kubeletFake) ConfigureKubelet(_ context.Context, _, pool string, settings map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings[pool] = settings
	return nil
}

func i32(n int32) *int32 { return &n }

// TestKubeletConfigRecreatesNodes (#2100): UpdateNodePool's kubelet_config
// is checked against how-to/node-system-config's ranges, each out of range
// INVALID_ARGUMENT; a valid one is an UPGRADE_NODES operation that records
// the settings as KubeletConfiguration fields (cpuManagerPolicy static
// with CPU reserved, a custom CFS period with its feature gate) and then
// replaces every node, so every node runs with them. The pool reads the
// config back. A pool made with node_config.kubelet_config has it from
// its first node.
func TestKubeletConfigRecreatesNodes(t *testing.T) {
	base := newUpFake()
	f := &kubeletFake{upFake: base, settings: map[string]map[string]any{}}
	s, err := NewServer(f, Options{Instance: "cloudburrow-t", AutoUpgradeInterval: -1,
		Versions: []Version{{GKE: newV, Image: newImg}, {GKE: oldV, Image: oldImg}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx := context.Background()
	name := makeOld(t, s, "kc", 2, nil, nil)
	pool := name + "/nodePools/default-pool"
	for what, kc := range map[string]*containerpb.NodeKubeletConfig{
		"cpu manager policy":        {CpuManagerPolicy: "dynamic"},
		"pod pids":                  {PodPidsLimit: 100},
		"image gc low over high":    {ImageGcLowThresholdPercent: 85, ImageGcHighThresholdPercent: 80},
		"image gc high":             {ImageGcHighThresholdPercent: 90},
		"image minimum age":         {ImageMinimumGcAge: "5m"},
		"image maximum under min":   {ImageMaximumGcAge: "1m"},
		"log size":                  {ContainerLogMaxSize: "5Mi"},
		"log files":                 {ContainerLogMaxFiles: 11},
		"sysctl":                    {AllowedUnsafeSysctls: []string{"vm.swappiness"}},
		"soft without grace":        {EvictionSoft: &containerpb.EvictionSignals{NodefsAvailable: "20%"}},
		"soft memory":               {EvictionSoft: &containerpb.EvictionSignals{MemoryAvailable: "50Mi"}, EvictionSoftGracePeriod: &containerpb.EvictionGracePeriod{MemoryAvailable: "1m"}},
		"soft nodefs range":         {EvictionSoft: &containerpb.EvictionSignals{NodefsAvailable: "60%"}, EvictionSoftGracePeriod: &containerpb.EvictionGracePeriod{NodefsAvailable: "1m"}},
		"grace too long":            {EvictionSoftGracePeriod: &containerpb.EvictionGracePeriod{PidAvailable: "5m"}},
		"reclaim":                   {EvictionMinimumReclaim: &containerpb.EvictionMinimumReclaim{PidAvailable: "10%"}},
		"max pod grace":             {EvictionMaxPodGracePeriodSeconds: 301},
		"parallel pulls":            {MaxParallelImagePulls: 6},
		"topology policy":           {TopologyManager: &containerpb.TopologyManager{Policy: "strict"}},
		"memory policy":             {MemoryManager: &containerpb.MemoryManager{Policy: "static"}},
		"shutdown":                  {ShutdownGracePeriodSeconds: i32(60)},
		"critical without shutdown": {ShutdownGracePeriodCriticalPodsSeconds: i32(10)},
		"critical over shutdown":    {ShutdownGracePeriodSeconds: i32(30), ShutdownGracePeriodCriticalPodsSeconds: i32(30)},
		"cfs period":                {CpuCfsQuotaPeriod: "2s"},
	} {
		_, err = s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, KubeletConfig: kc})
		wantCode(t, what, err, codes.InvalidArgument)
	}
	kc := &containerpb.NodeKubeletConfig{CpuManagerPolicy: "static", CpuCfsQuota: wrapperspb.Bool(false), CpuCfsQuotaPeriod: "50ms",
		PodPidsLimit: 4096, ContainerLogMaxSize: "20Mi", ContainerLogMaxFiles: 3, AllowedUnsafeSysctls: []string{"net.core.somaxconn", "kernel.shm*"},
		EvictionSoft:               &containerpb.EvictionSignals{MemoryAvailable: "200Mi", NodefsAvailable: "20%"},
		EvictionSoftGracePeriod:    &containerpb.EvictionGracePeriod{MemoryAvailable: "30s", NodefsAvailable: "1m"},
		ShutdownGracePeriodSeconds: i32(30), ShutdownGracePeriodCriticalPodsSeconds: i32(10),
		TopologyManager: &containerpb.TopologyManager{Policy: "best-effort", Scope: "pod"}}
	op, err := s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, KubeletConfig: kc})
	if err != nil {
		t.Fatal(err)
	}
	if op.GetOperationType() != containerpb.Operation_UPGRADE_NODES {
		t.Errorf("operation type = %s", op.GetOperationType())
	}
	if got := wait(t, s, "p", "us-central1-a", op); got.Error != nil {
		t.Fatal(got.Error)
	}
	if e := base.events(); strings.Count(e, "add ") != 2 || strings.Count(e, "remove ") != 2 {
		t.Errorf("events = %s, want both nodes replaced", e)
	}
	got := f.settings["default-pool"]
	for k, want := range map[string]any{"cpuManagerPolicy": "static", "cpuCFSQuota": false, "cpuCFSQuotaPeriod": "50ms", "podPidsLimit": int64(4096),
		"containerLogMaxSize": "20Mi", "containerLogMaxFiles": int32(3), "shutdownGracePeriod": "30s", "shutdownGracePeriodCriticalPods": "10s",
		"topologyManagerPolicy": "best-effort", "topologyManagerScope": "pod"} {
		if got[k] != want {
			t.Errorf("kubelet %s = %#v, want %#v", k, got[k], want)
		}
	}
	if got["kubeReserved"].(map[string]any)["cpu"] != "100m" || got["featureGates"].(map[string]any)["CustomCPUCFSQuotaPeriod"] != true ||
		got["evictionSoft"].(map[string]any)["memory.available"] != "200Mi" || got["evictionSoftGracePeriod"].(map[string]any)["nodefs.available"] != "1m" {
		t.Errorf("kubelet settings = %v", got)
	}
	np, _ := s.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if np.GetConfig().GetKubeletConfig().GetPodPidsLimit() != 4096 || np.GetStatus() != containerpb.NodePool_RUNNING {
		t.Errorf("pool after the update: %v %s", np.GetConfig().GetKubeletConfig(), np.GetStatus())
	}
	// The same config again recreates nothing.
	seen := base.events()
	op, err = s.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, KubeletConfig: kc})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p", "us-central1-a", op)
	if e := strings.TrimPrefix(base.events(), seen); strings.Contains(e, "add ") {
		t.Errorf("an unchanged kubelet config recreated nodes: %s", e)
	}

	cop, err := s.CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{Parent: name, NodePool: &containerpb.NodePool{Name: "pids", InitialNodeCount: 1,
		Config: &containerpb.NodeConfig{KubeletConfig: &containerpb.NodeKubeletConfig{PodPidsLimit: 2048}}}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, "p", "us-central1-a", cop)
	if f.settings["pids"]["podPidsLimit"] != int64(2048) {
		t.Errorf("a new pool's kubelet settings = %v", f.settings["pids"])
	}
	_, err = s.CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{Parent: name, NodePool: &containerpb.NodePool{Name: "bad", InitialNodeCount: 1,
		Config: &containerpb.NodeConfig{KubeletConfig: &containerpb.NodeKubeletConfig{PodPidsLimit: 10}}}})
	wantCode(t, "a new pool's pod pids limit of 10", err, codes.InvalidArgument)
}
