package doctor

import (
	"fmt"
	"slices"
	"strings"
)

// EngineKind names a container engine as `docker info` identifies it (#712).
type EngineKind string

// The engines CloudBurrow tells apart. docs/install.md, Container engines,
// states which of them are supported, unverified or unsupported, and why.
const (
	EngineDockerDesktop  EngineKind = "Docker Desktop"
	EngineDockerEngine   EngineKind = "Docker Engine"
	EngineRootlessDocker EngineKind = "rootless Docker"
	EnginePodman         EngineKind = "Podman"
	EngineColima         EngineKind = "colima"
	EngineOrbStack       EngineKind = "OrbStack"
	EngineRancherDesktop EngineKind = "Rancher Desktop"
	EngineLima           EngineKind = "lima"
	// EngineOtherVM is a daemon that is not on this host, because the host
	// is not Linux, but that none of the checks recognise.
	EngineOtherVM EngineKind = "an unrecognised VM engine"
)

// Engine is what the daemon is and where it runs.
type Engine struct {
	Kind EngineKind
	// InVM means the daemon, its filesystem and the kind network are inside
	// a VM, not on this host: DockerRootDir and the kind gateway are
	// addresses in the VM.
	InVM bool
	// Rootless means the daemon runs in a user namespace (rootlesskit or
	// rootless Podman), whose network the host does not share.
	Rootless bool
}

// ClassifyEngine reads the engine from `docker info` and the client's OS.
//
// The signals are the ones each engine documents: Podman's compat API adds
// BuildahVersion; Docker Desktop and OrbStack name themselves in
// OperatingSystem; colima and lima VMs are named by their hostname; rootless
// daemons list name=rootless in SecurityOptions. What is left is Docker
// Engine, on this host when the client is on Linux and in some VM otherwise.
func ClassifyEngine(info DockerInfo, goos string) Engine {
	rootless := info.Rootless || slices.ContainsFunc(info.SecurityOptions, func(o string) bool {
		return o == "name=rootless" || strings.HasPrefix(o, "name=rootless,")
	})
	nonLinux := goos != "linux"
	name := strings.ToLower(info.Name)
	switch {
	case info.BuildahVersion != "":
		// Podman runs natively on Linux and in a podman machine elsewhere.
		return Engine{Kind: EnginePodman, InVM: nonLinux, Rootless: rootless}
	case info.OperatingSystem == "Docker Desktop":
		return Engine{Kind: EngineDockerDesktop, InVM: true, Rootless: rootless}
	case info.OperatingSystem == "OrbStack" || name == "orbstack":
		return Engine{Kind: EngineOrbStack, InVM: true, Rootless: rootless}
	case name == "colima" || strings.HasPrefix(name, "colima-") || strings.HasPrefix(name, "lima-colima"):
		// colima always runs its daemon in a lima VM, on Linux too.
		return Engine{Kind: EngineColima, InVM: true, Rootless: rootless}
	case strings.HasPrefix(name, "lima-rancher-desktop") || strings.Contains(info.OperatingSystem, "Rancher Desktop"):
		return Engine{Kind: EngineRancherDesktop, InVM: true, Rootless: rootless}
	case strings.HasPrefix(name, "lima-"):
		return Engine{Kind: EngineLima, InVM: true, Rootless: rootless}
	case rootless:
		return Engine{Kind: EngineRootlessDocker, InVM: nonLinux, Rootless: true}
	case nonLinux:
		return Engine{Kind: EngineOtherVM, InVM: true}
	default:
		return Engine{Kind: EngineDockerEngine}
	}
}

// String is the engine as doctor and `up` name it.
func (e Engine) String() string {
	var where []string
	if e.InVM {
		where = append(where, "in a VM")
	}
	if e.Rootless && e.Kind != EngineRootlessDocker {
		where = append(where, "rootless")
	}
	if len(where) == 0 {
		if e.Kind == EngineDockerEngine {
			return "Docker Engine (rootful, on this host)"
		}
		return string(e.Kind)
	}
	return fmt.Sprintf("%s (%s)", e.Kind, strings.Join(where, ", "))
}

// Supported reports whether the engine is one docs/install.md lists as
// supported: Docker Desktop on macOS, and rootful Docker Engine on Linux.
func (e Engine) Supported(goos string) bool {
	switch e.Kind {
	case EngineDockerDesktop:
		return goos == "darwin" && !e.Rootless
	case EngineDockerEngine:
		return goos == "linux"
	}
	return false
}

// HostRelayRemedy is what to do when pods cannot reach this machine on e:
// host.docker.internal did not resolve in the kind node, so `up` would
// relay on the kind network's gateway, and on e that gateway is not an
// address on this host. Starting without Cloud Run publishes nothing.
func HostRelayRemedy(e Engine) string {
	const without = "or start without Cloud Run (`--services` without `run`), which publishes nothing to pods"
	switch {
	case e.Kind == EnginePodman:
		return "the kind network's gateway is inside Podman's VM or rootless network namespace, not on " +
			"this host, and Podman is unsupported; use Docker Desktop or rootful Docker Engine, " + without
	case e.Kind == EngineRootlessDocker || e.Rootless:
		return "the kind network's gateway is inside the rootless daemon's network namespace, not on " +
			"this host; use rootful Docker Engine or Docker Desktop, " + without
	case e.InVM:
		return fmt.Sprintf("%s runs the daemon in a VM, so the kind network's gateway is a VM address, and "+
			"host.docker.internal did not resolve inside the kind node; use Docker Desktop, or rootful "+
			"Docker Engine on Linux, %s", e.Kind, without)
	default:
		return "check that the kind network's gateway is an address on this host (`ip addr`), " + without
	}
}

// checkEngine names the engine and warns where the path from Cloud Run pods
// to this machine is not known to work (#712). It never blocks: the engine
// may well work, and `up` fails with the remedy if the relay cannot bind.
func checkEngine(info DockerInfo, infoErr error, goos string) Result {
	const name = "docker engine"
	if infoErr != nil {
		return Result{Name: name, Level: LevelUnknown, Detail: "not classified: `docker info` gave no answer",
			Remedy: "see docs/install.md, Container engines, for the engines CloudBurrow supports"}
	}
	e := ClassifyEngine(info, goos)
	detail := e.String()
	switch {
	case e.Supported(goos):
		return Result{Name: name, Level: LevelOK, Detail: detail}
	case e.Rootless || e.Kind == EnginePodman:
		return Result{Name: name, Level: LevelWarn, Detail: detail + "; unsupported: Cloud Run pods cannot reach this machine",
			Remedy: HostRelayRemedy(e)}
	case e.InVM && e.Kind != EngineDockerDesktop:
		return Result{Name: name, Level: LevelWarn,
			Detail: detail + "; unverified: Cloud Run pods reach this machine only if host.docker.internal resolves in the kind node",
			Remedy: "if `up` reports that the kind gateway is not bindable, " + HostRelayRemedy(e)}
	default:
		// Docker Desktop on Linux: the same forwarding as on macOS, never run.
		return Result{Name: name, Level: LevelWarn, Detail: detail + "; unverified on " + goos,
			Remedy: "it is expected to work as on macOS; see docs/install.md, Container engines"}
	}
}

// storagePath is dockerStoragePath: the daemon's root when it is a path on
// this host, and otherwise the home volume, labelled with what it stands for.
//
// On Linux DockerRootDir is trusted only when the daemon is not in a VM:
// Docker Desktop for Linux reports /var/lib/docker, a path inside its VM, and
// measuring the host's / under that name was a number with the wrong label
// (#712).
func storagePath(info DockerInfo, infoErr error, env Env) (string, string) {
	var e Engine
	if infoErr == nil {
		e = ClassifyEngine(info, env.GOOS)
		// A daemon root that exists on this host is the exact answer.
		if info.DockerRootDir != "" && env.GOOS == "linux" && !e.InVM {
			return info.DockerRootDir, ""
		}
	}
	home, err := env.HomeDir()
	if err != nil || home == "" {
		return "", ""
	}
	if infoErr != nil {
		if env.GOOS == "linux" {
			// Not known to be a VM, and not known to be this host either.
			return home, " (your home directory: the daemon did not say where it stores images)"
		}
		return home, " (the host volume backing the Docker VM disk, not the VM filesystem)"
	}
	vm := "the " + string(e.Kind) + " VM"
	if e.Kind == EngineOtherVM {
		vm = "the Docker VM"
	}
	return home, " (the host volume backing " + vm + " disk, not the VM filesystem)"
}
