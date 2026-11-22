package modelgarden

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/hfmodels"
	"github.com/cloudburrow/cloudburrow/internal/hostai"
)

// The Apple GPU (#2155). Docker Desktop's Linux VM gives the cluster's
// containers no access to the Apple GPU, so on a Mac with Apple silicon a
// deployment that asks for a GPU, and whose runtime has a Metal build
// (llama.cpp for GGUF, mlx-lm for MLX), runs as a supervised process of the
// host instead (internal/hostai), listening on 127.0.0.1 only. It is put in
// the cluster as a Service without a selector, named as its workload would
// be, whose EndpointSlice is the address pods reach the host at; the host
// reaches it directly, so :predict, :rawPredict and chat completions take
// the same path as to a cluster workload's tunnel.
//
// The accelerator is chosen in Google's terms, by the deploy's
// dedicated_resources.machine_spec: accelerator_type and accelerator_count.
// Google documents no Apple GPU, so CloudBurrow maps the GPU type Model
// Garden deploys small open models on, NVIDIA_L4, to it (and every other
// NVIDIA type too, so a sample written for one runs); a deploy with no
// machine_spec takes the instance's default (`up --local-ai-accelerator`).

// RuntimeHost is a deployed model served by a process of the host.
const RuntimeHost = "host"

// Accelerator choices of an instance (`up --local-ai-accelerator`).
const (
	AcceleratorMetal = "metal"
	AcceleratorCPU   = "cpu"
)

// MetalAcceleratorType is the documented accelerator type that maps to the
// Apple GPU, and MetalMachineType the machine type recorded for it when the
// deploy names none.
const (
	MetalAcceleratorType = "NVIDIA_L4"
	MetalMachineType     = "local-metal"
	CPUMachineType       = "local-cpu"
	// MetalLabel is how the console names the choice.
	MetalLabel = "Apple GPU (Metal), local"
)

// Host runs model servers as host processes on the Apple GPU.
type Host interface {
	// Metal reports whether the Apple GPU can be used, and why not.
	Metal() (bool, string)
	// Device names the GPU (for example "Apple M4 Max").
	Device() string
	// LlamaServer and MLXPython install the runtimes the first time and
	// return llama-server's path and the MLX venv's python.
	LlamaServer(ctx context.Context, progress func(string)) (string, error)
	MLXPython(ctx context.Context, progress func(string)) (string, error)
	// Start starts a supervised server.
	Start(spec hostai.Spec) (HostProcess, error)
	// Reap ends the servers a previous run left behind.
	Reap() []string
}

// HostProcess is a supervised host server.
type HostProcess interface {
	Wait(ctx context.Context, timeout time.Duration) error
	Healthy() bool
	Running() bool
	Tail(n int) string
	Stop(ctx context.Context) error
}

// HostAI is the Host of internal/hostai.
type HostAI struct {
	Installer *hostai.Installer
	Pids      *hostai.Pids
}

func (h *HostAI) Metal() (bool, string) { return hostai.Metal() }
func (h *HostAI) Device() string        { return hostai.Device() }
func (h *HostAI) LlamaServer(ctx context.Context, progress func(string)) (string, error) {
	return h.Installer.LlamaServer(ctx, progress)
}
func (h *HostAI) MLXPython(ctx context.Context, progress func(string)) (string, error) {
	return h.Installer.MLXPython(ctx, progress)
}
func (h *HostAI) Start(spec hostai.Spec) (HostProcess, error) {
	spec.Pids = h.Pids
	p, err := hostai.Start(spec)
	if err != nil {
		return nil, err
	}
	return p, nil
}
func (h *HostAI) Reap() []string {
	if h.Pids == nil {
		return nil
	}
	return h.Pids.Reap()
}

// LogLine is one line of a host server's output, for Cloud Logging.
type LogLine struct {
	Project, Location, Endpoint, DeployedModel string
	// Stream is stdout or stderr, or supervisor for CloudBurrow's own
	// events (started, ended, restarted).
	Stream, Text string
}

// hostEngine reports whether an engine has a Metal build the host runs.
func hostEngine(name string) bool { return name == EngineLlamaCpp || name == EngineMLX }

// Accel is a deploy's resolved accelerator.
type Accel struct {
	// Type and Count are the machine_spec's, as recorded and returned.
	Type  string
	Count int32
	// Metal is set when the deploy is to use the Apple GPU.
	Metal bool
	// Note says why a GPU that was asked for is not used.
	Note string
}

// resolveAccelerator reads the deploy's accelerator: a GPU type is the
// Apple GPU where there is one, CPU elsewhere (noted); a TPU type is
// refused; no machine_spec takes the instance default.
func (m *Manager) resolveAccelerator(r Request) (Accel, error) {
	t, n := strings.TrimSpace(r.AcceleratorType), r.AcceleratorCount
	metalOK, why := false, "this instance runs no host runtime"
	if m.Host != nil {
		metalOK, why = m.Host.Metal()
	}
	if t == "" || t == "ACCELERATOR_TYPE_UNSPECIFIED" {
		if n != 0 {
			return Accel{}, apierror.InvalidArgument("machine_spec.accelerator_count is %d but no accelerator_type is set", n)
		}
		if r.MachineType != "" {
			return Accel{}, nil
		}
		if m.defaultAccelerator() == AcceleratorMetal && metalOK {
			return Accel{Type: MetalAcceleratorType, Count: 1, Metal: true}, nil
		}
		return Accel{}, nil
	}
	if n < 0 {
		return Accel{}, apierror.InvalidArgument("machine_spec.accelerator_count must not be negative")
	}
	if n == 0 {
		n = 1
	}
	switch {
	case strings.HasPrefix(t, "TPU_"):
		return Accel{}, apierror.InvalidArgument("machine_spec.accelerator_type %s is not available: CloudBurrow runs no TPU. "+
			"A GPU type (%s) runs on the Apple GPU of an Apple silicon Mac, and on CPU elsewhere", t, MetalAcceleratorType)
	case strings.HasPrefix(t, "NVIDIA_"):
		if metalOK {
			return Accel{Type: t, Count: n, Metal: true}, nil
		}
		return Accel{Type: t, Count: n, Note: "Served on CPU: " + why}, nil
	}
	return Accel{}, apierror.InvalidArgument("machine_spec.accelerator_type %q is not an accelerator type of Agent Platform", t)
}

// defaultAccelerator is the instance's choice: metal when it is set so or
// left to the default on a Mac with Apple silicon, cpu otherwise.
func (m *Manager) defaultAccelerator() string {
	switch m.Accelerator {
	case AcceleratorCPU:
		return AcceleratorCPU
	case AcceleratorMetal:
		return AcceleratorMetal
	}
	if m.Host != nil {
		if ok, _ := m.Host.Metal(); ok {
			return AcceleratorMetal
		}
	}
	return AcceleratorCPU
}

// DefaultAccelerator is the accelerator a deploy with no machine_spec
// uses, for the console's form: MetalAcceleratorType, or "" for CPU.
func (m *Manager) DefaultAccelerator() string {
	if m.defaultAccelerator() == AcceleratorMetal && m.Host != nil {
		if ok, _ := m.Host.Metal(); ok {
			return MetalAcceleratorType
		}
	}
	return ""
}

// MetalAvailable reports whether deployments can use the Apple GPU here,
// why not, and the GPU's name.
func (m *Manager) MetalAvailable() (bool, string, string) {
	if m.Host == nil {
		return false, "this instance runs no host runtime", ""
	}
	ok, why := m.Host.Metal()
	if !ok {
		return false, why, ""
	}
	return true, "", m.Host.Device()
}

// hostFiles are the repository files a host deployment downloads: a GGUF
// model's one file; an MLX model's weights, configuration and tokenizer.
func hostFiles(engine, file string, files []hfmodels.File) []hfmodels.File {
	var out []hfmodels.File
	for _, f := range files {
		base := path.Base(f.Path)
		switch {
		case engine == EngineLlamaCpp:
			if f.Path == file {
				out = append(out, f)
			}
		case base == "README.md" || base == ".gitattributes" || strings.HasPrefix(f.Path, "."):
		case strings.HasSuffix(base, ".safetensors"), strings.HasSuffix(base, ".json"), strings.HasSuffix(base, ".txt"),
			strings.HasSuffix(base, ".model"), strings.HasSuffix(base, ".tiktoken"), strings.HasSuffix(base, ".jinja"), base == "merges.txt":
			out = append(out, f)
		}
	}
	return out
}

func (m *Manager) hostModelsDir() string {
	if m.HostModelsDir != "" {
		return m.HostModelsDir
	}
	return filepath.Join(filepath.Dir(m.StateFile), "host-models")
}

// hostModelDir is where a deployment's files are kept on the host: one
// directory per endpoint, removed when the model is undeployed.
func (m *Manager) hostModelDir(epID string) string {
	return filepath.Join(m.hostModelsDir(), workloadName(epID))
}

func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// HostCommand is the server a host deployment runs: llama-server with
// every layer on the GPU, or mlx-lm's server, on 127.0.0.1:port.
func HostCommand(engine, bin, dir string, model *Model, port int, contextLen int) (string, []string, []string) {
	env := []string{"HF_HUB_OFFLINE=1", "HF_HUB_DISABLE_TELEMETRY=1", "DO_NOT_TRACK=1"}
	switch engine {
	case EngineLlamaCpp:
		args := []string{"--host", "127.0.0.1", "--port", strconv.Itoa(port),
			"--model", filepath.Join(dir, filepath.FromSlash(model.File)), "--alias", model.HFModelID,
			"--n-gpu-layers", "999", "--no-webui",
			// Verbosity 4 logs the Metal device, the offloaded layers and the
			// GPU buffers, which Cloud Logging then shows.
			"--verbosity", "4"}
		if model.PipelineTag == "feature-extraction" || model.PipelineTag == "sentence-similarity" {
			args = append(args, "--embeddings")
		} else {
			n := maxModelLen
			if contextLen > 0 && contextLen < n {
				n = contextLen
			}
			args = append(args, "--ctx-size", strconv.Itoa(n))
		}
		return bin, args, env
	}
	return bin, []string{"-m", "mlx_lm", "server", "--model", dir, "--host", "127.0.0.1", "--port", strconv.Itoa(port)}, env
}

// runHost installs the runtime, downloads the model's files, starts its
// server on the Apple GPU and waits until it answers, then publishes it to
// the cluster.
func (m *Manager) runHost(ctx context.Context, project, location string, p Prepared, model *Model, name string) error {
	eng, _ := EngineByName(model.Engine)
	say := func(s string) { m.setProgress(project, location, p.Endpoint, s, 0) }
	var bin string
	var err error
	if model.Engine == EngineLlamaCpp {
		bin, err = m.Host.LlamaServer(ctx, say)
	} else {
		bin, err = m.Host.MLXPython(ctx, say)
	}
	if err != nil {
		return apierror.FailedPrecondition("installing %s for the Apple GPU: %v", eng.Title, err)
	}
	dir := m.hostModelDir(p.Endpoint)
	if _, err := os.Stat(filepath.Join(dir, ".cloudburrow-complete")); err != nil {
		files := model.HostFiles
		if len(files) == 0 {
			return apierror.FailedPrecondition("the deployment lists no files of %s to download", model.HFModelID)
		}
		say("Downloading " + model.HFModelID + " from Hugging Face")
		err := m.Models.FetchFiles(ctx, model.HFModelID, model.Revision, files, dir, p.token, func(done, total int64) {
			pct := 0
			if total > 0 {
				pct = int(done * 100 / total)
			}
			m.setProgress(project, location, p.Endpoint, fmt.Sprintf("Downloading %s from Hugging Face (%s of %s)",
				model.HFModelID, mb(done), mb(total)), pct)
		})
		if err != nil {
			if errors.Is(err, hfmodels.ErrDiskSpace) {
				return apierror.FailedPrecondition("%v", err)
			}
			return apierror.FailedPrecondition("downloading %s: %s", model.HFModelID, scrub(err.Error(), p.token))
		}
		if err := os.WriteFile(filepath.Join(dir, ".cloudburrow-complete"), []byte(model.Revision+"\n"), 0o644); err != nil {
			return apierror.Internal(err, "recording the download of %s: %v", model.HFModelID, err)
		}
	}
	// The context length caps --ctx-size, as on the cluster: from the
	// downloaded config.json, or a GGUF repository's own on the Hub.
	contextLen := 0
	if b, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		contextLen = ContextLength(b)
	} else if model.Engine == EngineLlamaCpp {
		if b, err := m.Models.ReadFile(ctx, model.HFModelID, model.Revision, "config.json", p.token, 1<<20); err == nil {
			contextLen = ContextLength(b)
		}
	}
	port, err := freeLoopbackPort()
	if err != nil {
		return apierror.Internal(err, "choosing a port for the model server: %v", err)
	}
	path, args, env := HostCommand(model.Engine, bin, dir, model, port, contextLen)
	m.mu.Lock()
	model.Args = append([]string{path}, args...)
	if ep := m.endpoints[key(project, location, p.Endpoint)]; ep != nil && ep.Deployed != nil {
		ep.Deployed.HostPort = port
	}
	m.save()
	m.mu.Unlock()
	say("Starting " + eng.Title + " on the Apple GPU (Metal)")
	proc, err := m.Host.Start(hostai.Spec{Name: name, Path: path, Args: args, Env: env, Dir: dir,
		Health: fmt.Sprintf("http://127.0.0.1:%d%s", port, eng.HealthRoute),
		Log: func(stream, line string) {
			if m.Log != nil {
				m.Log(LogLine{Project: project, Location: location, Endpoint: p.Endpoint, DeployedModel: p.DeployedModel,
					Stream: stream, Text: line})
			}
		}})
	if err != nil {
		return apierror.FailedPrecondition("starting %s: %v", eng.Title, err)
	}
	m.mu.Lock()
	if m.procs == nil {
		m.procs = map[string]HostProcess{}
	}
	if old := m.procs[name]; old != nil {
		m.mu.Unlock()
		_ = old.Stop(context.Background())
		m.mu.Lock()
	}
	m.procs[name] = proc
	m.mu.Unlock()
	say("Loading the model in " + eng.Title + " on the Apple GPU (Metal)")
	timeout := m.ReadyTimeout
	if timeout == 0 {
		timeout = 20 * time.Minute
	}
	if err := proc.Wait(ctx, timeout); err != nil {
		return apierror.FailedPrecondition("%s on the Apple GPU did not start: %v", eng.Title, err)
	}
	if m.Cluster != nil {
		if err := m.Cluster.PublishHost(ctx, name, port); err != nil && m.Log != nil {
			m.Log(LogLine{Project: project, Location: location, Endpoint: p.Endpoint, DeployedModel: p.DeployedModel,
				Stream: "supervisor", Text: "cloudburrow: the cluster cannot reach the model server: " + err.Error()})
		}
	}
	return nil
}

// stopHost ends a host deployment's server and, when forget is set (it is
// undeployed), its Service and its files.
func (m *Manager) stopHost(ctx context.Context, name, epID string, forget bool) error {
	m.mu.Lock()
	proc := m.procs[name]
	delete(m.procs, name)
	m.mu.Unlock()
	var err error
	if proc != nil {
		err = proc.Stop(ctx)
	}
	if !forget {
		return err
	}
	if m.Cluster != nil {
		if cerr := m.Cluster.Remove(ctx, name); cerr != nil && err == nil {
			err = cerr
		}
	}
	if epID != "" {
		dir := m.hostModelDir(epID)
		_ = os.RemoveAll(dir)
		_ = os.RemoveAll(dir + ".partial")
	}
	return err
}

// hostHealthy reports whether a host deployment's server answers.
func (m *Manager) hostHealthy(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.procs[name]
	return p != nil && p.Healthy()
}

// HostTail is a host deployment's last lines of output, for the console.
func (m *Manager) HostTail(name string, n int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.procs[name]; p != nil {
		return p.Tail(n)
	}
	return ""
}
