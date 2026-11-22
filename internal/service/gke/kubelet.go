package gke

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// KubeletConfigurer is a Backend whose nodes' kubelets take a node pool's
// kubelet_config (#2100): settings, as KubeletConfiguration fields, that
// the nodes the pool gets from then on run with. GKE applies a change by
// re-creating the nodes ("This change requires re-creating the nodes",
// how-to/node-system-config), which the operation does.
type KubeletConfigurer interface {
	ConfigureKubelet(ctx context.Context, kind, pool string, settings map[string]any) error
}

// kubeletSettings checks a NodeKubeletConfig against GKE's documented
// ranges (re-read 2026-11-22: how-to/node-system-config, "Kubelet
// configuration options") and returns it as KubeletConfiguration fields
// (kubelet.config.k8s.io/v1beta1). nil is no settings.
func kubeletSettings(field string, kc *containerpb.NodeKubeletConfig) (map[string]any, error) {
	if kc == nil {
		return nil, nil
	}
	bad := func(format string, a ...any) error {
		return status.Errorf(codes.InvalidArgument, field+"."+format, a...)
	}
	out := map[string]any{}
	gates := map[string]any{}
	reserved := map[string]any{}
	switch p := kc.GetCpuManagerPolicy(); p {
	case "", "none":
		if p != "" {
			out["cpuManagerPolicy"] = "none"
		}
	case "static":
		// The static policy hands whole CPUs out of what is not
		// reserved; GKE reserves CPU on every node, and the kubelet
		// refuses static with none reserved.
		out["cpuManagerPolicy"] = "static"
		reserved["cpu"] = "100m"
	default:
		return nil, bad("cpuManagerPolicy %q is not one of none or static.", p)
	}
	if q := kc.GetCpuCfsQuota(); q != nil {
		out["cpuCFSQuota"] = q.GetValue()
	}
	if p := kc.GetCpuCfsQuotaPeriod(); p != "" {
		d, err := time.ParseDuration(p)
		if err != nil || d < time.Millisecond || d > time.Second {
			return nil, bad("cpuCfsQuotaPeriod %q is not a duration between 1ms and 1s.", p)
		}
		out["cpuCFSQuotaPeriod"] = p
		if d != 100*time.Millisecond {
			gates["CustomCPUCFSQuotaPeriod"] = true
		}
	}
	if n := kc.GetPodPidsLimit(); n != 0 {
		if n < 1024 || n > 4194304 {
			return nil, bad("podPidsLimit %d is not between 1024 and 4194304.", n)
		}
		out["podPidsLimit"] = n
	}
	if kc.InsecureKubeletReadonlyPortEnabled != nil {
		port := 0
		if kc.GetInsecureKubeletReadonlyPortEnabled() {
			port = 10255
		}
		out["readOnlyPort"] = port
	}
	low, high := kc.GetImageGcLowThresholdPercent(), kc.GetImageGcHighThresholdPercent()
	for name, v := range map[string]int32{"imageGcLowThresholdPercent": low, "imageGcHighThresholdPercent": high} {
		if v != 0 && (v < 10 || v > 85) {
			return nil, bad("%s %d is not between 10 and 85.", name, v)
		}
	}
	if low != 0 || high != 0 {
		l, h := unsetInt(low, 80), unsetInt(high, 85)
		if l >= h {
			return nil, bad("imageGcLowThresholdPercent %d must be lower than imageGcHighThresholdPercent %d.", l, h)
		}
		out["imageGCLowThresholdPercent"], out["imageGCHighThresholdPercent"] = l, h
	}
	minAge := 2 * time.Minute
	if a := kc.GetImageMinimumGcAge(); a != "" {
		d, err := time.ParseDuration(a)
		if err != nil || d < 0 || d > 2*time.Minute {
			return nil, bad("imageMinimumGcAge %q is not a duration of at most 2m.", a)
		}
		minAge = d
		out["imageMinimumGCAge"] = a
	}
	if a := kc.GetImageMaximumGcAge(); a != "" {
		d, err := time.ParseDuration(a)
		if err != nil || d < 0 || d != 0 && d <= minAge {
			return nil, bad("imageMaximumGcAge %q must be a duration greater than imageMinimumGcAge (%s).", a, minAge)
		}
		out["imageMaximumGCAge"] = a
	}
	if sz := kc.GetContainerLogMaxSize(); sz != "" {
		q, ok := quantity(sz)
		unit := strings.TrimLeft(sz, "0123456789")
		if !ok || (unit != "Ki" && unit != "Mi" && unit != "Gi") || q < 10<<20 || q > 500<<20 {
			return nil, bad("containerLogMaxSize %q is not between 10Mi and 500Mi, in Ki, Mi or Gi.", sz)
		}
		out["containerLogMaxSize"] = sz
	}
	if n := kc.GetContainerLogMaxFiles(); n != 0 {
		if n < 2 || n > 10 {
			return nil, bad("containerLogMaxFiles %d is not between 2 and 10.", n)
		}
		out["containerLogMaxFiles"] = n
	}
	if ss := kc.GetAllowedUnsafeSysctls(); len(ss) > 0 {
		for _, s := range ss {
			if !unsafeSysctlAllowed(s) {
				return nil, bad("allowedUnsafeSysctls %q is not in kernel.shm*, kernel.msg*, kernel.sem, fs.mqueue.* or net.*.", s)
			}
		}
		out["allowedUnsafeSysctls"] = ss
	}
	soft, err := evictionSignals(kc.GetEvictionSoft(), bad)
	if err != nil {
		return nil, err
	}
	grace, err := evictionGrace(kc.GetEvictionSoftGracePeriod(), bad)
	if err != nil {
		return nil, err
	}
	for sig := range soft {
		if _, ok := grace[sig]; !ok {
			return nil, bad("evictionSoft %s has no evictionSoftGracePeriod.", sig)
		}
	}
	if len(soft) > 0 {
		out["evictionSoft"] = soft
	}
	if len(grace) > 0 {
		out["evictionSoftGracePeriod"] = grace
	}
	reclaim, err := evictionReclaim(kc.GetEvictionMinimumReclaim(), bad)
	if err != nil {
		return nil, err
	}
	if len(reclaim) > 0 {
		out["evictionMinimumReclaim"] = reclaim
	}
	if n := kc.GetEvictionMaxPodGracePeriodSeconds(); n != 0 {
		if n < 0 || n > 300 {
			return nil, bad("evictionMaxPodGracePeriodSeconds %d is not between 0 and 300.", n)
		}
		out["evictionMaxPodGracePeriod"] = n
	}
	if n := kc.GetMaxParallelImagePulls(); n != 0 {
		if n < 2 || n > 5 {
			return nil, bad("maxParallelImagePulls %d is not between 2 and 5.", n)
		}
		out["maxParallelImagePulls"] = n
		out["serializeImagePulls"] = false
	}
	if kc.SingleProcessOomKill != nil {
		out["singleProcessOOMKill"] = kc.GetSingleProcessOomKill()
	}
	if tm := kc.GetTopologyManager(); tm != nil {
		switch p := tm.GetPolicy(); p {
		case "":
		case "none", "best-effort", "restricted", "single-numa-node":
			out["topologyManagerPolicy"] = p
		default:
			return nil, bad("topologyManager.policy %q is not one of none, best-effort, restricted or single-numa-node.", p)
		}
		switch sc := tm.GetScope(); sc {
		case "":
		case "container", "pod":
			out["topologyManagerScope"] = sc
		default:
			return nil, bad("topologyManager.scope %q is not one of container or pod.", sc)
		}
	}
	if mm := kc.GetMemoryManager(); mm != nil {
		switch p := mm.GetPolicy(); p {
		case "":
		case "None":
			out["memoryManagerPolicy"] = "None"
		case "Static":
			// Static needs the memory it may not hand out reserved,
			// and reservedMemory equal to that reservation and the
			// hard eviction threshold, set to the kubelet's default
			// 100Mi (merged into the nodes' evictionHard).
			out["memoryManagerPolicy"] = "Static"
			out["evictionHard"] = map[string]any{"memory.available": "100Mi"}
			reserved["memory"] = "100Mi"
			out["reservedMemory"] = []any{map[string]any{"numaNode": 0, "limits": map[string]any{"memory": "200Mi"}}}
		default:
			return nil, bad("memoryManager.policy %q is not one of None or Static.", p)
		}
	}
	if kc.ShutdownGracePeriodSeconds != nil {
		n := kc.GetShutdownGracePeriodSeconds()
		if n != 0 && n != 30 && n != 120 {
			return nil, bad("shutdownGracePeriodSeconds %d is not 0, 30 or 120.", n)
		}
		out["shutdownGracePeriod"] = fmt.Sprintf("%ds", n)
	}
	if kc.ShutdownGracePeriodCriticalPodsSeconds != nil {
		n := kc.GetShutdownGracePeriodCriticalPodsSeconds()
		if kc.ShutdownGracePeriodSeconds == nil {
			return nil, bad("shutdownGracePeriodCriticalPodsSeconds is only valid with shutdownGracePeriodSeconds.")
		}
		if n < 0 || n > 120 || n >= kc.GetShutdownGracePeriodSeconds() && n != 0 {
			return nil, bad("shutdownGracePeriodCriticalPodsSeconds %d must be between 0 and 120 and less than shutdownGracePeriodSeconds.", n)
		}
		out["shutdownGracePeriodCriticalPods"] = fmt.Sprintf("%ds", n)
	}
	if p := kc.GetCrashLoopBackOff().GetMaxContainerRestartPeriod(); p != "" {
		d, err := time.ParseDuration(p)
		if err != nil || d < time.Second || d > 300*time.Second {
			return nil, bad("crashLoopBackOff.maxContainerRestartPeriod %q is not a duration between 1s and 300s.", p)
		}
		out["crashLoopBackOff"] = map[string]any{"maxContainerRestartPeriod": p}
		gates["KubeletCrashLoopBackOffMax"] = true
	}
	if len(gates) > 0 {
		out["featureGates"] = gates
	}
	if len(reserved) > 0 {
		out["kubeReserved"] = reserved
	}
	return out, nil
}

// quantityRE is a Kubernetes memory quantity: a number and an optional
// binary or decimal suffix.
var quantityRE = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)(Ki|Mi|Gi|Ti|k|M|G|T)?$`)

// quantity reads a memory quantity in bytes.
func quantity(v string) (float64, bool) {
	m := quantityRE.FindStringSubmatch(v)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.ParseFloat(m[1], 64)
	mult := map[string]float64{"": 1, "Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "k": 1e3, "M": 1e6, "G": 1e9, "T": 1e12}[m[2]]
	return n * mult, true
}

func unsetInt(v, def int32) int32 {
	if v == 0 {
		return def
	}
	return v
}

// unsafeSysctlAllowed is GKE's allowlist: "kernel.shm*, kernel.msg*,
// kernel.sem, fs.mqueue.*, net.*" (how-to/node-system-config).
func unsafeSysctlAllowed(s string) bool {
	switch {
	case s == "kernel.sem", s == "kernel.shm*", s == "kernel.msg*", s == "fs.mqueue.*", s == "net.*":
		return true
	case strings.HasPrefix(s, "kernel.shm"), strings.HasPrefix(s, "kernel.msg"):
		return len(s) > len("kernel.shm")
	case strings.HasPrefix(s, "fs.mqueue."), strings.HasPrefix(s, "net."):
		return !strings.HasSuffix(s, ".")
	}
	return false
}

// evictionSignal is one of NodeKubeletConfig's eviction signals: its
// field, the kubelet's signal, and GKE's soft threshold range.
type evictionSignal struct {
	field, signal string
	get           func(sigs) string
	min, max      int // percent; memory is a quantity
}

// sigs is EvictionSignals, EvictionGracePeriod and EvictionMinimumReclaim,
// which share their fields.
type sigs interface {
	GetMemoryAvailable() string
	GetNodefsAvailable() string
	GetNodefsInodesFree() string
	GetImagefsAvailable() string
	GetImagefsInodesFree() string
	GetPidAvailable() string
}

var evictionSignalList = []evictionSignal{
	{"memoryAvailable", "memory.available", sigs.GetMemoryAvailable, 0, 0},
	{"nodefsAvailable", "nodefs.available", sigs.GetNodefsAvailable, 10, 50},
	{"nodefsInodesFree", "nodefs.inodesFree", sigs.GetNodefsInodesFree, 5, 50},
	{"imagefsAvailable", "imagefs.available", sigs.GetImagefsAvailable, 15, 50},
	{"imagefsInodesFree", "imagefs.inodesFree", sigs.GetImagefsInodesFree, 5, 50},
	{"pidAvailable", "pid.available", sigs.GetPidAvailable, 10, 50},
}

// percent reads "N%" (N may have a fraction).
func percent(v string) (float64, bool) {
	n, err := strconv.ParseFloat(strings.TrimSuffix(v, "%"), 64)
	return n, err == nil && strings.HasSuffix(v, "%")
}

// evictionSignals checks evictionSoft: memoryAvailable a quantity above
// 100Mi, the others percentages in their documented ranges.
func evictionSignals(s *containerpb.EvictionSignals, bad func(string, ...any) error) (map[string]any, error) {
	out := map[string]any{}
	if s == nil {
		return out, nil
	}
	for _, sig := range evictionSignalList {
		v := sig.get(s)
		if v == "" {
			continue
		}
		if sig.signal == "memory.available" {
			if q, ok := quantity(v); !ok || q <= 100<<20 {
				return nil, bad("evictionSoft.memoryAvailable %q is not a quantity above 100Mi.", v)
			}
		} else if n, ok := percent(v); !ok || n < float64(sig.min) || n > float64(sig.max) {
			return nil, bad("evictionSoft.%s %q is not a percentage between %d%% and %d%%.", sig.field, v, sig.min, sig.max)
		}
		out[sig.signal] = v
	}
	return out, nil
}

// evictionGrace checks evictionSoftGracePeriod: each positive and under 5m.
func evictionGrace(s *containerpb.EvictionGracePeriod, bad func(string, ...any) error) (map[string]any, error) {
	out := map[string]any{}
	if s == nil {
		return out, nil
	}
	for _, sig := range evictionSignalList {
		v := sig.get(s)
		if v == "" {
			continue
		}
		if d, err := time.ParseDuration(v); err != nil || d <= 0 || d >= 5*time.Minute {
			return nil, bad("evictionSoftGracePeriod.%s %q is not a positive duration under 5m.", sig.field, v)
		}
		out[sig.signal] = v
	}
	return out, nil
}

// evictionReclaim checks evictionMinimumReclaim: each a positive
// percentage under 10%.
func evictionReclaim(s *containerpb.EvictionMinimumReclaim, bad func(string, ...any) error) (map[string]any, error) {
	out := map[string]any{}
	if s == nil {
		return out, nil
	}
	for _, sig := range evictionSignalList {
		v := sig.get(s)
		if v == "" {
			continue
		}
		if n, ok := percent(v); !ok || n <= 0 || n >= 10 {
			return nil, bad("evictionMinimumReclaim.%s %q is not a positive percentage under 10%%.", sig.field, v)
		}
		out[sig.signal] = v
	}
	return out, nil
}

// configureKubelet records a pool's kubelet settings for the nodes it gets
// from then on; a backend with no kubelets of its own does nothing.
func (s *Server) configureKubelet(ctx context.Context, kind, pool string, settings map[string]any) error {
	kc, ok := s.backend.(KubeletConfigurer)
	if !ok {
		return nil
	}
	if settings == nil {
		settings = map[string]any{}
	}
	if err := kc.ConfigureKubelet(ctx, kind, pool, settings); err != nil {
		return status.Errorf(codes.Internal, "Setting node pool %q's kubelet configuration failed: %v", pool, err)
	}
	return nil
}
