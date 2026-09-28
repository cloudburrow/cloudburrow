// Package doctor diagnoses a workstation before CloudBurrow tries to create a
// cluster.
//
// Everything it inspects is a real cause of a confusing failure later: a
// Docker VM too small for Knative reports itself as pods stuck Pending, a
// taken port reports itself as a bind error thirty seconds into startup, and
// a missing binary reports itself as an exec error from inside a component.
// Diagnosing them up front turns each into one sentence with a fix.
//
// Every probe is injected, so the whole package is testable without Docker, a
// cluster or a network.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Level is how much a finding matters.
//
// The distinction is the whole point of the command: a warning is something
// that will probably still work, and a failure is something that will not.
// Collapsing them would make the exit code meaningless.
type Level int

const (
	// LevelOK means the check passed.
	LevelOK Level = iota
	// LevelWarn means it will probably work, with a caveat worth knowing.
	LevelWarn
	// LevelFail means `cloudburrow up` will not succeed until it is fixed.
	LevelFail
	// LevelUnknown means the check could not be performed. It is deliberately
	// not OK: an unanswerable question is not a passing answer.
	LevelUnknown
)

func (l Level) String() string {
	switch l {
	case LevelOK:
		return "ok"
	case LevelWarn:
		return "warn"
	case LevelFail:
		return "FAIL"
	default:
		return "unknown"
	}
}

// Result is one finding.
type Result struct {
	Name   string
	Level  Level
	Detail string
	// Remedy is what to do about it. Required for anything but LevelOK: a
	// diagnosis without a fix just moves the problem.
	Remedy string
}

// Report is the outcome of a run.
type Report struct {
	Results []Result
}

// Blocking reports whether anything will stop `cloudburrow up`.
//
// Warnings and unknowns do not block. A preflight that refused to proceed
// because it could not measure something would be worse than no preflight:
// developers would stop running it.
func (r Report) Blocking() bool {
	for _, res := range r.Results {
		if res.Level == LevelFail {
			return true
		}
	}
	return false
}

// Write renders the report.
func (r Report) Write(w io.Writer) {
	for _, res := range r.Results {
		fmt.Fprintf(w, "  %-7s %-24s %s\n", res.Level, res.Name, res.Detail)
		if res.Remedy != "" && res.Level != LevelOK {
			fmt.Fprintf(w, "          %s\n", res.Remedy)
		}
	}

	var warn, fail, unknown int
	for _, res := range r.Results {
		switch res.Level {
		case LevelWarn:
			warn++
		case LevelFail:
			fail++
		case LevelUnknown:
			unknown++
		}
	}
	fmt.Fprintln(w)
	switch {
	case fail > 0:
		fmt.Fprintf(w, "%d blocking problem(s); `cloudburrow up` will not succeed until they are fixed.\n", fail)
	case warn > 0 || unknown > 0:
		fmt.Fprintf(w, "No blocking problems. %d warning(s), %d not determined.\n", warn, unknown)
	default:
		fmt.Fprintln(w, "All checks passed.")
	}
}

// DockerInfo is the subset of `docker info` the checks read.
type DockerInfo struct {
	// NCPU is the CPU count available to the daemon.
	NCPU int `json:"NCPU"`
	// MemTotal is the daemon's total memory in bytes.
	MemTotal int64 `json:"MemTotal"`
	// DockerRootDir is where the daemon stores images and containers. On
	// macOS it is a path inside the VM and does not exist on the host.
	DockerRootDir string `json:"DockerRootDir"`
	// ServerVersion identifies the daemon.
	ServerVersion string `json:"ServerVersion"`
	// OperatingSystem is the daemon's host OS, e.g. "Docker Desktop".
	OperatingSystem string `json:"OperatingSystem"`
	// Name is the daemon host's hostname: "colima" or "orbstack" on those
	// engines' VMs.
	Name string `json:"Name"`
	// SecurityOptions lists name=rootless on a rootless daemon.
	SecurityOptions []string `json:"SecurityOptions"`
	// BuildahVersion and Rootless are Podman's additions to the Docker
	// compat API; Docker never sends them.
	BuildahVersion string `json:"BuildahVersion"`
	Rootless       bool   `json:"Rootless"`
}

// Env is the injected probe surface.
type Env struct {
	// LookPath resolves a binary, as exec.LookPath does.
	LookPath func(string) (string, error)
	// Run executes a command and returns its combined output.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// PortFree reports whether a TCP port can be bound on the given host.
	// A nil error means free.
	PortFree func(host string, port int) error
	// DiskFree returns free bytes on the filesystem holding path, and the
	// path actually measured.
	DiskFree func(path string) (uint64, string, error)
	// HomeDir is the user's home directory, used as the stand-in for the
	// volume backing a Docker VM disk image.
	HomeDir func() (string, error)
	// GOOS is the host operating system.
	GOOS string
	// Reach reports whether an HTTPS URL answers at all: any HTTP response
	// is reachable, a failed connection is not. Nil leaves reachability
	// undetermined, which is reported as unknown rather than skipped.
	Reach func(ctx context.Context, url string) error
}

// Options configure a run.
type Options struct {
	// BindAddress is where CloudBurrow will publish host endpoints.
	BindAddress string
	// Ports are the host ports CloudBurrow will bind, by name. A port of 0 is
	// OS-assigned and is not checked, because there is nothing to collide with.
	Ports map[string]int
	// Fixed names ports that cannot be OS-assigned, so the remedy does not
	// offer an escape that would quietly disable the feature instead.
	Fixed map[string]bool
}

// Requirements that a smaller machine fails.
//
// The memory figure is not a guess: `docker info` on a machine that could not
// start Knative reported less. Knative Serving's controller, webhook,
// activator and autoscaler, plus kourier, plus the CloudBurrow backends, do
// not fit in less than about 4 GiB with room for workloads. The footprint
// run in docs/status.md measured the node container at 1321 MiB for the
// default services and 2535 MiB with every opt-in one, so 4 GiB leaves room
// for workloads in the first case and less in the second.
const (
	// MinDockerMemoryBytes is the daemon memory below which startup fails.
	MinDockerMemoryBytes int64 = 4 << 30
	// MinDockerCPUs is the CPU count below which startup is unreliable.
	MinDockerCPUs = 2
	// MinFreeDiskBytes covers the node image, Knative, the backends and room
	// for the images a developer builds.
	MinFreeDiskBytes uint64 = 20 << 30
)

// Pinned tool versions. These duplicate dependencies.json, which remains the
// source of truth; #31 keeps them in step.
const (
	// KindVersion is the kind release CloudBurrow is verified against.
	KindVersion = "v0.33.0"
)

// Run performs every check and returns the report.
//
// Checks are independent: one failure never stops the rest, because a
// developer fixing three problems wants to see all three.
func Run(ctx context.Context, env Env, opts Options) Report {
	// `docker info` is asked once and shared. Asking twice would double the
	// cost of the slowest probe and could report two different answers.
	info, raw, err := dockerInfo(ctx, env)

	var r Report
	r.Results = append(r.Results, checkBinaries(ctx, env)...)
	r.Results = append(r.Results, checkDockerDaemon(info, raw, err)...)
	r.Results = append(r.Results, checkEngine(info, err, env.GOOS))
	r.Results = append(r.Results, checkDisk(info, err, env))
	r.Results = append(r.Results, checkPorts(env, opts)...)
	r.Results = append(r.Results, checkReachability(ctx, env)...)
	return r
}

// Endpoint is a host a first `up` downloads from.
type Endpoint struct {
	Name string
	URL  string
	// For is what `up` fetches there.
	For string
}

// Endpoints are every host a first `up` downloads from (#604): the
// registries of the node, backend and Knative images, and GitHub for
// Knative's release YAMLs. A registry answers /v2/ with 401 without
// credentials, which counts as reachable.
var Endpoints = []Endpoint{
	{"registry docker.io", "https://registry-1.docker.io/v2/", "kind node image, PostgreSQL, MySQL, Valkey, Envoy"},
	{"registry gcr.io", "https://gcr.io/v2/", "Cloud SDK emulators, Spanner, distroless, Knative images"},
	{"registry ghcr.io", "https://ghcr.io/v2/", "BigQuery emulator"},
	{"github.com", "https://github.com/", "Knative release YAMLs"},
}

// checkReachability reports each endpoint as reachable, unreachable or
// unknown, one row each, never silently skipped.
//
// Unreachable is a warning, not a failure: `up` does not need the network
// when `cloudburrow prefetch` has stored what it would download, and a
// cluster that already exists has its images.
func checkReachability(ctx context.Context, env Env) []Result {
	out := make([]Result, len(Endpoints))
	var wg sync.WaitGroup
	for i, e := range Endpoints {
		name := "reach " + e.Name
		if env.Reach == nil {
			out[i] = Result{Name: name, Level: LevelUnknown, Detail: "unknown: not probed",
				Remedy: "check by hand that " + e.URL + " answers; `up` downloads the " + e.For + " there"}
			continue
		}
		wg.Add(1)
		go func(i int, e Endpoint) {
			defer wg.Done()
			err := env.Reach(ctx, e.URL)
			switch {
			case err == nil:
				out[i] = Result{Name: name, Level: LevelOK, Detail: "reachable"}
			case ctx.Err() != nil:
				out[i] = Result{Name: name, Level: LevelUnknown, Detail: "unknown: " + ctx.Err().Error(),
					Remedy: "check by hand that " + e.URL + " answers"}
			default:
				out[i] = Result{Name: name, Level: LevelWarn, Detail: "unreachable: " + err.Error(),
					Remedy: "`up` downloads the " + e.For + " from here unless they are cached: run " +
						"`cloudburrow prefetch` where it is reachable, then `cloudburrow up --offline` " +
						"(docs/install.md, Offline and air-gapped use)"}
			}
		}(i, e)
	}
	wg.Wait()
	return out
}

// Ports performs only the port checks. `up` runs it before creating a
// cluster, so a taken port is refused in a second rather than found when its
// listener starts, after the cluster exists.
func Ports(env Env, opts Options) Report {
	return Report{Results: checkPorts(env, opts)}
}

// dockerInfo asks the daemon once. The raw output is returned too, so a parse
// failure can be reported against what was actually received.
func dockerInfo(ctx context.Context, env Env) (DockerInfo, []byte, error) {
	out, err := env.Run(ctx, "docker", "info", "--format", "{{json .}}")
	if err != nil {
		return DockerInfo{}, out, err
	}
	var info DockerInfo
	if jsonErr := json.Unmarshal(out, &info); jsonErr != nil {
		return DockerInfo{}, out, jsonErr
	}
	return info, out, nil
}

// requiredBinaries are the tools CloudBurrow executes directly.
var requiredBinaries = []string{"docker", "kind", "kubectl"}

func checkBinaries(ctx context.Context, env Env) []Result {
	var out []Result
	for _, bin := range requiredBinaries {
		path, err := env.LookPath(bin)
		if err != nil {
			out = append(out, Result{
				Name:   bin,
				Level:  LevelFail,
				Detail: "not found on PATH",
				Remedy: installHint(bin),
			})
			continue
		}
		detail := path
		level := LevelOK
		remedy := ""
		if bin == "kind" {
			if got := kindVersion(ctx, env); got == "" {
				detail = path + " (version not reported)"
				level = LevelUnknown
				remedy = "could not run `kind version`; CloudBurrow is verified against " + KindVersion
			} else if got != KindVersion {
				detail = fmt.Sprintf("%s (%s)", path, got)
				// A different kind is a warning, not a failure: it may work
				// perfectly well. Claiming it will not would be inventing a
				// result nothing measured.
				level = LevelWarn
				remedy = "CloudBurrow is verified against kind " + KindVersion +
					"; other releases are untested, not known-broken"
			} else {
				detail = fmt.Sprintf("%s (%s)", path, got)
			}
		}
		out = append(out, Result{Name: bin, Level: level, Detail: detail, Remedy: remedy})
	}
	return out
}

func installHint(bin string) string {
	switch bin {
	case "docker":
		return "install Docker Desktop or the Docker Engine, and make sure the daemon is running"
	case "kind":
		return "install kind " + KindVersion + ": https://kind.sigs.k8s.io/docs/user/quick-start/#installation"
	case "kubectl":
		return "install kubectl: https://kubernetes.io/docs/tasks/tools/"
	default:
		return "install " + bin
	}
}

var kindVersionRE = regexp.MustCompile(`v\d+\.\d+\.\d+`)

func kindVersion(ctx context.Context, env Env) string {
	out, err := env.Run(ctx, "kind", "version")
	if err != nil {
		return ""
	}
	return kindVersionRE.FindString(string(out))
}

func checkDockerDaemon(info DockerInfo, raw []byte, err error) []Result {
	if err != nil {
		// A daemon that answered but spoke nonsense is a different problem
		// from one that did not answer: only the second stops `up`.
		if len(raw) > 0 && json.Valid(raw) {
			return []Result{{
				Name:   "docker daemon",
				Level:  LevelUnknown,
				Detail: "`docker info` output could not be read: " + err.Error(),
				Remedy: "check `docker info --format '{{json .}}'` by hand",
			}}
		}
		return []Result{{
			Name:   "docker daemon",
			Level:  LevelFail,
			Detail: "not reachable: " + firstLine(string(raw), err),
			Remedy: "start Docker and re-run; every CloudBurrow component runs in it",
		}}
	}

	results := []Result{{
		Name:   "docker daemon",
		Level:  LevelOK,
		Detail: fmt.Sprintf("%s (%s)", info.ServerVersion, info.OperatingSystem),
	}}

	switch {
	case info.MemTotal <= 0:
		results = append(results, Result{
			Name:   "docker memory",
			Level:  LevelUnknown,
			Detail: "the daemon did not report its memory",
			Remedy: fmt.Sprintf("allocate at least %s by hand", human(uint64(MinDockerMemoryBytes))),
		})
	case info.MemTotal < MinDockerMemoryBytes:
		results = append(results, Result{
			Name:  "docker memory",
			Level: LevelFail,
			Detail: fmt.Sprintf("%s available, %s required",
				human(uint64(info.MemTotal)), human(uint64(MinDockerMemoryBytes))),
			Remedy: "raise the memory limit in Docker Desktop → Settings → Resources; " +
				"below this Knative's control plane is OOM-killed and pods stay Pending",
		})
	default:
		results = append(results, Result{
			Name:   "docker memory",
			Level:  LevelOK,
			Detail: human(uint64(info.MemTotal)),
		})
	}

	switch {
	case info.NCPU <= 0:
		results = append(results, Result{
			Name:   "docker cpus",
			Level:  LevelUnknown,
			Detail: "the daemon did not report its CPU count",
			Remedy: fmt.Sprintf("allocate at least %d by hand", MinDockerCPUs),
		})
	case info.NCPU < MinDockerCPUs:
		results = append(results, Result{
			Name:   "docker cpus",
			Level:  LevelFail,
			Detail: fmt.Sprintf("%d available, %d required", info.NCPU, MinDockerCPUs),
			Remedy: "raise the CPU limit in Docker Desktop → Settings → Resources",
		})
	default:
		results = append(results, Result{
			Name:   "docker cpus",
			Level:  LevelOK,
			Detail: fmt.Sprintf("%d", info.NCPU),
		})
	}
	return results
}

// checkDisk measures free space where it can actually be measured.
//
// With the daemon on this host (Linux, not in a VM) its root directory is a
// host path and the answer is exact. On macOS, and on Linux with a VM engine
// such as Docker Desktop for Linux (#712), it is a path inside the VM,
// invisible from the host, so what is measured instead is the volume holding
// the VM's disk image — the real constraint, since that image grows into it. The difference
// is stated rather than hidden, because a number whose meaning is unclear is
// worse than a number labelled honestly.
func checkDisk(info DockerInfo, infoErr error, env Env) Result {
	path, note := dockerStoragePath(info, infoErr, env)
	if path == "" {
		return Result{
			Name:   "disk space",
			Level:  LevelUnknown,
			Detail: "no filesystem could be measured",
			Remedy: fmt.Sprintf("make sure at least %s is free for images and cluster state",
				human(MinFreeDiskBytes)),
		}
	}

	free, measured, err := env.DiskFree(path)
	if err != nil {
		return Result{
			Name:   "disk space",
			Level:  LevelUnknown,
			Detail: fmt.Sprintf("could not measure %s: %v", path, err),
			Remedy: fmt.Sprintf("make sure at least %s is free", human(MinFreeDiskBytes)),
		}
	}

	detail := fmt.Sprintf("%s free on %s%s", human(free), measured, note)
	if free < MinFreeDiskBytes {
		return Result{
			Name:   "disk space",
			Level:  LevelWarn,
			Detail: detail + fmt.Sprintf(", below the %s margin", human(MinFreeDiskBytes)),
			Remedy: "free space or run `docker image prune` yourself; CloudBurrow never " +
				"prunes Docker on your behalf",
		}
	}
	return Result{Name: "disk space", Level: LevelOK, Detail: detail}
}

// dockerStoragePath returns a path to measure and a note explaining what it
// represents.
func dockerStoragePath(info DockerInfo, infoErr error, env Env) (string, string) {
	return storagePath(info, infoErr, env)
}

func checkPorts(env Env, opts Options) []Result {
	host := opts.BindAddress
	if host == "" {
		host = "127.0.0.1"
	}

	names := make([]string, 0, len(opts.Ports))
	for n := range opts.Ports {
		names = append(names, n)
	}
	// Sorted so two runs on the same machine print the same thing.
	slices.Sort(names)

	var out []Result
	for _, name := range names {
		port := opts.Ports[name]
		if port == 0 {
			// OS-assigned: there is nothing to collide with, so reporting a
			// result would be noise.
			continue
		}
		if err := env.PortFree(host, port); err != nil {
			remedy := fmt.Sprintf("stop whatever holds it, or pass --port-%s 0 to let the OS choose", name)
			if opts.Fixed[name] {
				// Offering `0` here would be wrong: for a fixed port it
				// disables the feature rather than moving it.
				remedy = fmt.Sprintf("stop whatever holds it, or choose another port with --port-%s", name)
			}
			out = append(out, Result{
				Name:   "port " + name,
				Level:  LevelFail,
				Detail: fmt.Sprintf("%s:%d is in use", host, port),
				Remedy: remedy,
			})
			continue
		}
		out = append(out, Result{
			Name:   "port " + name,
			Level:  LevelOK,
			Detail: fmt.Sprintf("%s:%d is free", host, port),
		})
	}
	return out
}

// human renders a byte count the way Docker Desktop shows it.
func human(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit && exp < 3; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGT"[exp])
}

// firstLine keeps a diagnosis to one line. `docker info` on a stopped daemon
// prints a paragraph, and burying the cause in it helps nobody.
func firstLine(out string, err error) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "Client:") {
			return line
		}
	}
	return err.Error()
}

// EmbeddedStorageFix is how to get a CLI with the storage server in it, or
// start without one.
const EmbeddedStorageFix = "build with `make build` (or `make storage-binaries` before `go build`), " +
	"install a release (docs/install.md), or pass --services without storage and pubsub"

// EmbeddedStorage reports whether this CLI embeds the builtin Cloud Storage
// server (#686), for which Linux architectures, and whether one is the
// node's. missing maps each architecture a build could embed to "" when it
// is present, or to why it is not; nodeArch is the Docker daemon's, which a
// kind node shares.
//
// Only the node's architecture decides: `up` builds the storage image from
// that build alone, so a CLI missing the other one still starts. With
// storage disabled nothing is built and a missing server does not block.
// storageEnabled is whether anything runs from the image: Cloud Storage's
// server, or Pub/Sub's front (#873).
func EmbeddedStorage(storageEnabled bool, nodeArch string, missing map[string]string) Result {
	const name = "embedded storage"
	var present, absent []string
	for arch, why := range missing {
		if why == "" {
			present = append(present, "linux/"+arch)
		} else {
			absent = append(absent, "linux/"+arch)
		}
	}
	slices.Sort(present)
	slices.Sort(absent)
	has := "none embedded"
	if len(present) > 0 {
		has = strings.Join(present, ", ") + " embedded"
	}
	why, known := missing[nodeArch]
	if !known {
		why = "no linux/" + nodeArch + " build is ever embedded"
	}
	switch {
	case why == "":
		detail := fmt.Sprintf("%s, matching the node (linux/%s)", has, nodeArch)
		if len(absent) > 0 {
			detail += "; not " + strings.Join(absent, ", ")
		}
		return Result{Name: name, Level: LevelOK, Detail: detail}
	case !storageEnabled:
		return Result{Name: name, Level: LevelOK,
			Detail: fmt.Sprintf("%s; not needed, neither Cloud Storage nor Pub/Sub is enabled", has)}
	default:
		return Result{Name: name, Level: LevelFail,
			Detail: fmt.Sprintf("%s; none for the node (linux/%s): %s. This CLI was built without the "+
				"embedded storage server, so `up` would refuse to start Cloud Storage or Pub/Sub", has, nodeArch, why),
			Remedy: EmbeddedStorageFix}
	}
}
