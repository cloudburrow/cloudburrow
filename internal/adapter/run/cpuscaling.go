package run

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
)

// CPU-target scaling (#1789, #1800): Cloud Run's CPU utilization driver.
//
// Google's description (https://docs.cloud.google.com/run/docs/about-instance-autoscaling,
// read 2026-11-18): Cloud Run "calculates the number of instances by
// averaging the CPU usage per second over a 1-minute period", "divides this
// by the number of CPUs per instance" and then "by the CPU target"; the
// count holds "within a 10% tolerance of your target"; each driver is
// computed "independently" and Cloud Run scales to "whichever scaling
// driver requires more instances"; the default target is 60%.
//
// Locally the CPU signal is metrics-server's metrics.k8s.io (installed by
// up beside Knative), whose usage is a rate over its 15 s resolution. Every
// cpuInterval the controller reads each scaled group's pods, computes
//
//	desired = ceil(sum(usage) / (cpus per instance * target))
//
// (unchanged inside the 10% tolerance), keeps the highest recommendation of
// the last cpuDownWindow so a short dip does not scale in, and applies it:
//
//   - a service's revision: as a floor under KPA's own concurrency
//     recommendation, written into the revision's min-scale with the
//     service-level split (applyServiceScaling), capped by its max-scale.
//     KPA then runs the larger of the two, which is "whichever requires
//     more". An idle revision's CPU floor is 0, so scale to zero is kept.
//   - an AUTOMATIC worker pool's revision with a cpuUtilization: the
//     Deployment's replicas, between its share of minInstanceCount and
//     maxInstanceCount (cpuPoolCeiling without one).
//
// CPUs per instance are the container's CPU limit, Cloud Run's default of
// 1 when none is set.

const (
	cpuInterval   = 10 * time.Second
	cpuDownWindow = 60 * time.Second
	cpuTolerance  = 0.1
	// defaultCPUTarget is Cloud Run's 60%, for a template that sets none.
	defaultCPUTarget = 0.6
	// cpuPoolCeiling bounds an AUTOMATIC pool with no maxInstanceCount.
	cpuPoolCeiling = 100
)

// cpuScaler is the controller's state: each group's recent
// recommendations, and each service revision's current CPU floor.
type cpuScaler struct {
	mu     sync.Mutex
	recs   map[string][]cpuRec
	floors map[string]int32
	now    func() time.Time
}

type cpuRec struct {
	at time.Time
	n  int32
}

func newCPUScaler() *cpuScaler {
	return &cpuScaler{recs: map[string][]cpuRec{}, floors: map[string]int32{}, now: time.Now}
}

// stabilized records n for key and returns the highest recommendation
// within the scale-down window: scale out at once, scale in only once the
// window has passed without a higher one.
func (c *cpuScaler) stabilized(key string, n int32) int32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	keep := c.recs[key][:0]
	for _, r := range c.recs[key] {
		if now.Sub(r.at) < cpuDownWindow {
			keep = append(keep, r)
		}
	}
	keep = append(keep, cpuRec{now, n})
	c.recs[key] = keep
	best := int32(0)
	for _, r := range keep {
		if r.n > best {
			best = r.n
		}
	}
	return best
}

// floor is a service revision's CPU floor; 0 when it has none.
func (c *cpuScaler) floor(revision string) int32 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.floors[revision]
}

func (c *cpuScaler) setFloor(revision string, n int32) (changed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.floors[revision] == n {
		return false
	}
	if n == 0 {
		delete(c.floors, revision)
	} else {
		c.floors[revision] = n
	}
	return true
}

// cpuGroup is one scaled group's pods as the signal sees them.
type cpuGroup struct {
	pods     int     // running pods, not being deleted
	measured int     // of them, those metrics-server has a sample for
	usage    float64 // CPUs in use, summed over measured pods
	perPod   float64 // CPUs allocated to an instance
}

// cpuRecommendation is the CPU driver's instance count for g at target,
// and false when it has no opinion (no pods, or a pod not yet measured
// while utilization is below target: a new pod would read as idle).
func cpuRecommendation(g cpuGroup, target float64) (int32, bool) {
	if g.pods == 0 || g.measured == 0 || target <= 0 || g.perPod <= 0 {
		return 0, g.pods == 0
	}
	util := g.usage / (float64(g.measured) * g.perPod)
	if math.Abs(util/target-1) <= cpuTolerance {
		return int32(g.pods), true
	}
	n := int32(math.Ceil(g.usage / (g.perPod * target)))
	if g.measured < g.pods && n < int32(g.pods) {
		return int32(g.pods), true
	}
	return n, true
}

// parseCPU reads a Kubernetes CPU quantity ("250m", "1", "1234567n") as
// CPUs.
func parseCPU(q string) (float64, error) {
	q = strings.TrimSpace(q)
	scale := 1.0
	switch {
	case strings.HasSuffix(q, "n"):
		scale, q = 1e-9, strings.TrimSuffix(q, "n")
	case strings.HasSuffix(q, "u"):
		scale, q = 1e-6, strings.TrimSuffix(q, "u")
	case strings.HasSuffix(q, "m"):
		scale, q = 1e-3, strings.TrimSuffix(q, "m")
	}
	f, err := strconv.ParseFloat(q, 64)
	if err != nil {
		return 0, fmt.Errorf("CPU quantity %q: %w", q, err)
	}
	return f * scale, nil
}

// sidecars are containers CloudBurrow or Knative runs beside the user's,
// whose CPU is not the instance's.
func sidecar(name string) bool { return name == "queue-proxy" || name == "cloud-sql-proxy" }

// readCPUGroup reads the pods selector matches and their metrics.
func (s *Server) readCPUGroup(ctx context.Context, selector string) (cpuGroup, error) {
	var g cpuGroup
	raw, err := s.kn.Kube.List(ctx, "pods", selector, "json")
	if err != nil {
		return g, err
	}
	var pods struct {
		Items []struct {
			Metadata struct {
				Name              string  `json:"name"`
				DeletionTimestamp *string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Spec struct {
				Containers []struct {
					Name      string `json:"name"`
					Resources struct {
						Limits map[string]string `json:"limits"`
					} `json:"resources"`
				} `json:"containers"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &pods); err != nil {
		return g, fmt.Errorf("decode pods: %w", err)
	}
	live := map[string]bool{}
	for _, p := range pods.Items {
		if p.Metadata.DeletionTimestamp != nil || p.Status.Phase != "Running" {
			continue
		}
		live[p.Metadata.Name] = true
		if g.perPod == 0 {
			for _, c := range p.Spec.Containers {
				if sidecar(c.Name) {
					continue
				}
				cpus := 1.0 // Cloud Run's default CPU
				if v, ok := c.Resources.Limits["cpu"]; ok {
					if f, err := parseCPU(v); err == nil && f > 0 {
						cpus = f
					}
				}
				g.perPod += cpus
			}
		}
	}
	g.pods = len(live)
	if g.pods == 0 {
		return g, nil
	}
	raw, err = s.kn.Kube.List(ctx, "pods.metrics.k8s.io", selector, "json")
	if err != nil {
		return g, err
	}
	var metrics struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Containers []struct {
				Name  string            `json:"name"`
				Usage map[string]string `json:"usage"`
			} `json:"containers"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &metrics); err != nil {
		return g, fmt.Errorf("decode pod metrics: %w", err)
	}
	for _, m := range metrics.Items {
		if !live[m.Metadata.Name] {
			continue
		}
		g.measured++
		for _, c := range m.Containers {
			if sidecar(c.Name) {
				continue
			}
			if f, err := parseCPU(c.Usage["cpu"]); err == nil {
				g.usage += f
			}
		}
	}
	return g, nil
}

// cpuTarget is a template's CPU target: its cpuUtilization, Cloud Run's
// 60% when unset, 0 when disabled.
func cpuTarget(set *float32) float64 {
	if set == nil {
		return defaultCPUTarget
	}
	return float64(*set)
}

// StartCPUScaling runs the CPU driver until ctx ends (an instance calls it;
// tests drive scaleOnCPU themselves).
func (s *Server) StartCPUScaling(ctx context.Context) {
	if s.cpu == nil {
		s.cpu = newCPUScaler()
	}
	go func() {
		t := time.NewTicker(cpuInterval)
		defer t.Stop()
		warned := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if err := s.scaleOnCPU(ctx); err != nil && ctx.Err() == nil && !warned {
				// Once: without metrics-server every pass fails the same way.
				slog.Warn("Cloud Run could not scale on CPU utilization", "err", err)
				warned = true
			} else if err == nil {
				warned = false
			}
		}
	}()
}

// scaleOnCPU is one pass over services and worker pools.
func (s *Server) scaleOnCPU(ctx context.Context) error {
	errS := s.scaleServicesOnCPU(ctx)
	errP := s.scalePoolsOnCPU(ctx)
	if errS != nil {
		return errS
	}
	return errP
}

func (s *Server) scaleServicesOnCPU(ctx context.Context) error {
	svcs, err := s.kn.List(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, k := range svcs {
		revs, err := s.kn.Revisions(ctx, k.Metadata.Name)
		if err != nil {
			firstErr = orErr(firstErr, err)
			continue
		}
		changed := false
		for _, share := range trafficShares(k) {
			var target float64
			for _, r := range revs {
				if r.Metadata.Name == share.revision {
					target = cpuTarget(readRevisionScaling(r.Metadata.Annotations).CpuUtilization)
				}
			}
			n := int32(0)
			if target > 0 {
				g, err := s.readCPUGroup(ctx, "serving.knative.dev/revision="+share.revision)
				if err != nil {
					firstErr = orErr(firstErr, err)
					continue
				}
				rec, ok := cpuRecommendation(g, target)
				if !ok {
					rec = s.cpu.floor(share.revision)
				}
				n = s.cpu.stabilized("rev/"+share.revision, rec)
			}
			if s.cpu.setFloor(share.revision, n) {
				changed = true
			}
		}
		if changed {
			if err := s.applyServiceScaling(ctx, k); err != nil {
				firstErr = orErr(firstErr, err)
			}
		}
	}
	return firstErr
}

func (s *Server) scalePoolsOnCPU(ctx context.Context) error {
	cms, err := s.specsOfKind(ctx, kindWorkerPool)
	if err != nil {
		return err
	}
	var firstErr error
	for _, cm := range cms {
		var wp runpb.WorkerPool
		if err := decodeSpec(cm, &wp); err != nil {
			continue
		}
		// A pool with no cpuUtilization sets no CPU threshold (docs/gaps.md:
		// Google documents no default for worker pools), and stays at its
		// minimum.
		p := scalingOf(wp.GetScaling())
		if !p.automatic() || p.cpu == nil || *p.cpu <= 0 {
			continue
		}
		target := float64(*p.cpu)
		name := cm.Metadata.Annotations[annRunName]
		w := workloadOf(kindWorkerPool, name[strings.LastIndex(name, "/")+1:], name, cm)
		latestID := revisionID(wp.GetLatestCreatedRevision())
		maxTotal := p.max
		if maxTotal <= 0 {
			maxTotal = cpuPoolCeiling
		}
		mins := splitShares(wp.GetInstanceSplits(), latestID, p.min)
		maxs := splitShares(wp.GetInstanceSplits(), latestID, maxTotal)
		for i, sh := range mins {
			dep := w.objectName()
			if i > 0 {
				dep = splitDeploymentName(w, sh.revID)
			}
			g, err := s.readCPUGroup(ctx, labelRevision+"="+sh.revID)
			if err != nil {
				firstErr = orErr(firstErr, err)
				continue
			}
			rec, ok := cpuRecommendation(g, target)
			if !ok {
				continue
			}
			n := s.cpu.stabilized("pool/"+dep, rec)
			lo, hi := sh.count, maxs[i].count
			if hi < lo {
				hi = lo
			}
			n = min(max(n, lo), hi)
			if int(n) == g.pods {
				continue
			}
			patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, n)
			if err := s.kn.Kube.Patch(ctx, "deployment", dep, patch); err != nil {
				firstErr = orErr(firstErr, err)
			}
		}
	}
	return firstErr
}

func orErr(first, err error) error {
	if first != nil {
		return first
	}
	return err
}
