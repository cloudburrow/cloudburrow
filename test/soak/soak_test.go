//go:build soak

// Package soak drives one running instance with the official Go clients at a
// steady, realistic mix for a long time, and reports what drifted: errors,
// latency percentiles, memory, threads, file descriptors, restarts, the state
// directory's size, and the delivery guarantees of Pub/Sub and Tasks.
//
// It is behind the "soak" build tag, so no default test run, and no CI job,
// builds it. docs/soak.md says how to run it.
package soak

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The instance's endpoints, as scripts/compat-env.sh exports them.
const (
	envStorage   = "CLOUDBURROW_TEST_STORAGE"
	envPubSub    = "CLOUDBURROW_TEST_PUBSUB"
	envTasks     = "CLOUDBURROW_TEST_TASKS"
	envSecrets   = "CLOUDBURROW_TEST_SECRETS"
	envFirestore = "CLOUDBURROW_TEST_FIRESTORE"
	envBigQuery  = "CLOUDBURROW_TEST_BIGQUERY"
	envBQProject = "CLOUDBURROW_TEST_BIGQUERY_PROJECT"
	envLogging   = "CLOUDBURROW_TEST_LOGGING"
)

// The soak's own settings.
const (
	envDuration = "CLOUDBURROW_SOAK_DURATION" // default 30m
	envGroups   = "CLOUDBURROW_SOAK_GROUPS"   // default every group
	envRate     = "CLOUDBURROW_SOAK_RATE"     // multiplier on every worker's rate; default 1
	envWindow   = "CLOUDBURROW_SOAK_WINDOW"   // latency window; default 1m
	envReport   = "CLOUDBURROW_SOAK_REPORT"   // JSON report path
	envPID      = "CLOUDBURROW_SOAK_PID"      // the `up` process to sample
	envNode     = "CLOUDBURROW_SOAK_NODE"     // the kind node container to sample
	envStateDir = "CLOUDBURROW_SOAK_STATE_DIR"
	// name=address pairs, comma-separated, of processes serving pprof
	// (CLOUDBURROW_DEBUG_PPROF, #1609), whose goroutines are sampled.
	envPprof = "CLOUDBURROW_SOAK_PPROF"
)

// maxClients bounds the concurrent clients a run opens, so a soak keeps the
// machine usable for everything else on it.
const maxClients = 32

// env is what every workload shares.
type env struct {
	t       *testing.T
	project string
	suffix  string
	rate    float64
	rec     *recorder
}

// workload is one service group.
type workload struct {
	name    string
	clients int
	// run starts the group's workers, which stop when ctx is done; the
	// returned check runs after they all have, and verifies what can only be
	// verified at the end (nothing lost, nothing delivered twice).
	run func(ctx context.Context, e *env, wg *sync.WaitGroup) (check func(), err error)
}

var workloads = []workload{
	{"storage", 4, runStorage},
	{"pubsub", 4, runPubSub},
	{"tasks", 2, runTasks},
	{"secretmanager", 2, runSecrets},
	{"firestore", 4, runFirestore},
	{"bigquery", 2, runBigQuery},
	{"logging", 3, runLogging},
}

func TestSoak(t *testing.T) {
	for _, v := range []string{"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT"} {
		if os.Getenv(v) != "" {
			t.Fatalf("%s is set; the soak refuses to run with cloud credentials in the environment", v)
		}
	}
	duration := durationEnv(t, envDuration, 30*time.Minute)
	window := durationEnv(t, envWindow, time.Minute)
	rate := 1.0
	if v := os.Getenv(envRate); v != "" {
		var err error
		if rate, err = strconv.ParseFloat(v, 64); err != nil || rate <= 0 {
			t.Fatalf("%s=%q is not a positive number", envRate, v)
		}
	}
	want := map[string]bool{}
	for _, g := range strings.Split(os.Getenv(envGroups), ",") {
		if g = strings.TrimSpace(g); g != "" {
			want[g] = true
		}
	}

	suffix := strconv.FormatInt(time.Now().Unix(), 36)
	e := &env{t: t, project: "cb-soak-" + suffix, suffix: suffix, rate: rate, rec: newRecorder(window)}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var wg sync.WaitGroup
	var checks []func()
	clients := 0
	for _, w := range workloads {
		if len(want) > 0 && !want[w.name] {
			continue
		}
		if clients+w.clients > maxClients {
			t.Fatalf("group %s would take the run past %d clients", w.name, maxClients)
		}
		check, err := w.run(ctx, e, &wg)
		if err != nil {
			t.Errorf("%s: setup: %v", w.name, err)
			continue
		}
		if check == nil { // its endpoint is not served
			continue
		}
		clients += w.clients
		checks = append(checks, check)
		t.Logf("%s: started %d clients", w.name, w.clients)
	}
	if len(checks) == 0 {
		t.Skip("no group's endpoint is set; export the instance's (see docs/soak.md)")
	}

	smp := &sampler{hostPID: os.Getenv(envPID), node: os.Getenv(envNode), stateDir: os.Getenv(envStateDir),
		pprof: pprofTargets(t, os.Getenv(envPprof))}
	sctx, scancel := context.WithCancel(context.Background())
	sdone := make(chan struct{})
	go func() { smp.run(sctx, e.rec.start, 30*time.Second); close(sdone) }()

	wg.Wait()
	for _, check := range checks {
		check()
	}
	scancel()
	<-sdone

	reps := e.rec.report()
	samples := smp.all()
	for _, r := range reps {
		t.Logf("%-22s ops %7d  errors %4d (%.4f%%)  first p50/p95/p99 %6.1f/%6.1f/%6.1f ms  last %6.1f/%6.1f/%6.1f ms",
			r.Group, r.Ops, r.Errors, 100*r.ErrorRate,
			r.First.P50ms, r.First.P95ms, r.First.P99ms, r.Last.P50ms, r.Last.P95ms, r.Last.P99ms)
		for code, n := range r.ByCode {
			t.Errorf("%s: %d errors %s, such as %q", r.Group, n, code, r.Samples[code])
		}
		if r.Drift != "" {
			t.Logf("%s: latency drifted: %s", r.Group, r.Drift)
		}
	}
	if len(samples) > 1 {
		first, last := samples[0], samples[len(samples)-1]
		t.Logf("host: rss %d -> %d KB, threads %d -> %d, fds %d -> %d; state %d -> %d bytes",
			first.HostRSSKB, last.HostRSSKB, first.HostThr, last.HostThr, first.HostFDs, last.HostFDs, first.StateBytes, last.StateBytes)
		for k, c := range last.Containers {
			f := first.Containers[k]
			t.Logf("%-22s anon %d -> %d KB, threads %d -> %d, fds %d -> %d, restarts %d -> %d",
				k, f.AnonKB, c.AnonKB, f.Threads, c.Threads, f.FDs, c.FDs, f.Restarts, c.Restarts)
			if c.Restarts > f.Restarts {
				t.Errorf("%s restarted %d times during the soak", k, c.Restarts-f.Restarts)
			}
		}
		for k, n := range last.Goroutines {
			t.Logf("%-22s goroutines %d -> %d", k, first.Goroutines[k], n)
		}
	}
	if path := os.Getenv(envReport); path != "" {
		b, _ := json.MarshalIndent(map[string]any{"duration": duration.String(), "rate": rate, "groups": reps, "samples": samples}, "", "  ")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Errorf("write report: %v", err)
		}
	}
}

func durationEnv(t *testing.T, name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		t.Fatalf("%s=%q is not a positive duration", name, v)
	}
	return d
}

// endpoint returns a service's local endpoint, or "" when it is not set. It
// refuses anything that is not loopback, so a stray variable can never point
// the soak at Google.
func endpoint(t *testing.T, name string) string {
	addr := strings.TrimSpace(os.Getenv(name))
	if addr == "" {
		t.Logf("%s is not set; its group does not run", name)
		return ""
	}
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(addr, "http://"), "https://"), "/")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		t.Fatalf("%s=%q is not loopback; the soak only talks to local instances", name, addr)
	}
	return addr
}

// pprofTargets parses envPprof's name=address pairs, refusing an address
// that is not loopback.
func pprofTargets(t *testing.T, v string) map[string]string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(v, ",") {
		name, addr, ok := strings.Cut(strings.TrimSpace(pair), "=")
		host, _, err := net.SplitHostPort(addr)
		if ip := net.ParseIP(host); !ok || name == "" || err != nil || ip == nil || !ip.IsLoopback() {
			t.Fatalf("%s: %q is not name=<loopback ip>:<port>", envPprof, pair)
		}
		out[name] = addr
	}
	return out
}

// every calls fn rate times a second, scaled by the run's multiplier, until
// ctx is done. A call that overruns its slot delays the next, rather than
// stacking calls up: the load stays what was asked for, and a slow service
// shows as latency, not as a queue inside the harness.
func (e *env) every(ctx context.Context, wg *sync.WaitGroup, rate float64, fn func(ctx context.Context)) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		interval := time.Duration(float64(time.Second) / (rate * e.rate))
		// Workers start out of phase, so a group's calls do not arrive in step.
		time.Sleep(time.Duration(rand.Int64N(int64(interval) + 1)))
		t := time.NewTicker(interval)
		defer t.Stop()
		for ctx.Err() == nil {
			fn(ctx)
			select {
			case <-ctx.Done():
			case <-t.C:
			}
		}
	}()
}

// call bounds one operation, independent of the run's context, so the last
// call of a run is not recorded as an error because the run ended under it.
func call() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 60*time.Second)
}

func label(prefix string, n int) string { return fmt.Sprintf("%s-%d", prefix, n) }
