package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// `cloudburrow logs` (#280): emulator, component and Cloud Run workload logs
// in the terminal.
//
// Pod logs reached a developer only through the console's Logs Explorer,
// which needs a browser and holds 2000 entries, and the services `up` runs
// in-process logged only to its own terminal. This reads both, from the
// instance's own kubeconfig and nothing else, and passes every line through
// the console's credential redaction.

type logsOptions struct {
	service, resource, format string
	follow                    bool
	since                     time.Duration
	tail                      int
}

// logLine is one line from any source.
type logLine struct {
	Time     time.Time `json:"time"`
	Source   string    `json:"source"`
	Resource string    `json:"resource,omitempty"`
	Message  string    `json:"message"`
}

// kubectl runs kubectl against the instance's cluster only.
//
// It is the one way this file reaches kubectl. The instance's kubeconfig is
// passed explicitly on every call, and KUBECONFIG is removed from the child's
// environment, so no other context — a developer's production cluster
// included — can be read through a missing flag.
type kubectl struct {
	kubeconfig string
	// run and stream are replaceable so tests can see every argument list.
	run    func(ctx context.Context, args []string) ([]byte, error)
	stream func(ctx context.Context, args []string) (io.ReadCloser, func() error, error)
}

func newKubectl(kubeconfig string) *kubectl {
	env := func() []string {
		var out []string
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "KUBECONFIG=") {
				out = append(out, kv)
			}
		}
		return out
	}
	return &kubectl{
		kubeconfig: kubeconfig,
		run: func(ctx context.Context, args []string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, "kubectl", args...)
			cmd.Env = env()
			return cmd.Output()
		},
		stream: func(ctx context.Context, args []string) (io.ReadCloser, func() error, error) {
			cmd := exec.CommandContext(ctx, "kubectl", args...)
			cmd.Env = env()
			out, err := cmd.StdoutPipe()
			if err != nil {
				return nil, nil, err
			}
			if err := cmd.Start(); err != nil {
				return nil, nil, err
			}
			return out, cmd.Wait, nil
		},
	}
}

func (k *kubectl) args(rest ...string) []string {
	return append([]string{"--kubeconfig", k.kubeconfig}, rest...)
}

// inProcess are the services `up` runs itself; their logs are up.log.
var inProcess = map[string]bool{"tasks": true, "secretmanager": true, "metadata": true, "cloudburrow": true}

func runLogs(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	opts, rest, err := logsFlags(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return errUsage
	}
	cfg, err := config.Load(config.Options{Args: rest, Output: stderr})
	if err != nil {
		return err
	}
	return streamLogs(ctx, cfg, opts, newKubectl(cfg.KubeconfigPath()), stdout)
}

func logsFlags(args []string) (logsOptions, []string, error) {
	o := logsOptions{format: "text", tail: 100}
	var v string
	var found bool
	var err error
	rest := args
	if v, _, rest, err = splitFlag(rest, "service", false); err != nil {
		return o, nil, err
	}
	o.service = v
	if v, _, rest, err = splitFlag(rest, "resource", false); err != nil {
		return o, nil, err
	}
	o.resource = v
	if v, found, rest, err = splitFlag(rest, "follow", true); err != nil {
		return o, nil, err
	}
	o.follow = found && v != "false"
	if v, _, rest, err = splitFlag(rest, "since", false); err != nil {
		return o, nil, err
	}
	if v != "" {
		if o.since, err = time.ParseDuration(v); err != nil || o.since <= 0 {
			return o, nil, fmt.Errorf("invalid -since %q: want a duration such as 10m", v)
		}
	}
	if v, found, rest, err = splitFlag(rest, "tail", false); err != nil {
		return o, nil, err
	}
	if found {
		if o.tail, err = strconv.Atoi(v); err != nil || o.tail < 0 {
			return o, nil, fmt.Errorf("invalid -tail %q: want a line count", v)
		}
	}
	if v, found, rest, err = splitFlag(rest, "format", false); err != nil {
		return o, nil, err
	}
	if found {
		if v != "text" && v != "json" {
			return o, nil, fmt.Errorf("invalid -format %q: want text or json", v)
		}
		o.format = v
	}
	if o.resource != "" && o.service != string(config.ServiceRun) {
		return o, nil, errors.New("-resource names a Cloud Run service, so it needs -service run")
	}
	if o.service != "" && !inProcess[o.service] {
		known := false
		for _, s := range config.KnownServices() {
			known = known || string(s) == o.service
		}
		if !known {
			return o, nil, fmt.Errorf("unknown -service %q", o.service)
		}
	}
	return o, rest, nil
}

// source is one stream of lines.
type source struct {
	name, resource string
	open           func(ctx context.Context) (io.ReadCloser, func() error, error)
}

func streamLogs(ctx context.Context, cfg config.Config, o logsOptions, k *kubectl, stdout io.Writer) error {
	if err := instanceRunning(ctx, cfg, k); err != nil {
		return err
	}
	sources, err := logSources(ctx, cfg, o, k)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return fmt.Errorf("no logs to show for %s: nothing of this instance's is running for it", describeSelection(o))
	}

	lines := make(chan logLine, 256)
	var wg sync.WaitGroup
	for _, src := range sources {
		wg.Add(1)
		go func(src source) {
			defer wg.Done()
			r, wait, err := src.open(ctx)
			if err != nil {
				return
			}
			defer func() { _ = r.Close(); _ = wait() }()
			sc := bufio.NewScanner(r)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				ts, msg := splitTimestamp(sc.Text())
				if o.since > 0 && !ts.IsZero() && time.Since(ts) > o.since {
					continue
				}
				select {
				case lines <- logLine{ts, src.name, src.resource, console.Redact(msg)}:
				case <-ctx.Done():
					return
				}
			}
		}(src)
	}
	go func() { wg.Wait(); close(lines) }()

	write := func(l logLine) {
		if o.format == "json" {
			b, _ := json.Marshal(l)
			fmt.Fprintln(stdout, string(b))
			return
		}
		stamp := "                        "
		if !l.Time.IsZero() {
			stamp = l.Time.Format("2006-01-02T15:04:05.000Z")
		}
		who := l.Source
		if l.Resource != "" {
			who += " " + l.Resource
		}
		fmt.Fprintf(stdout, "%s %s: %s\n", stamp, who, l.Message)
	}

	if o.follow {
		for l := range lines {
			write(l)
		}
		if ctx.Err() != nil {
			// Interrupted is how a follow ends; 130 is the shell's code for it.
			return &exitError{code: 130, err: errors.New("interrupted"), quiet: true}
		}
		return nil
	}
	// Not following: every source has ended, so the lines can be put in
	// time order across sources, which is how someone reads a failure.
	var all []logLine
	for l := range lines {
		all = append(all, l)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Time.Before(all[j].Time) })
	for _, l := range all {
		write(l)
	}
	return nil
}

func describeSelection(o logsOptions) string {
	switch {
	case o.resource != "":
		return "Cloud Run service " + o.resource
	case o.service != "":
		return o.service
	default:
		return "this instance"
	}
}

// instanceRunning refuses a stopped or absent instance with the reason,
// rather than letting kubectl print a connection error about a kubeconfig
// the developer has never heard of.
func instanceRunning(ctx context.Context, cfg config.Config, k *kubectl) error {
	if _, err := os.Stat(k.kubeconfig); err != nil {
		return fmt.Errorf("instance %q is not running: it has no cluster (%s does not exist); start it with `cloudburrow up`",
			cfg.Name, k.kubeconfig)
	}
	if _, err := k.run(ctx, k.args("get", "namespace", cfg.Cluster.Namespace, "-o", "name")); err != nil {
		return fmt.Errorf("instance %q is not running: its cluster does not answer; start it with `cloudburrow up`", cfg.Name)
	}
	return nil
}

func logSources(ctx context.Context, cfg config.Config, o logsOptions, k *kubectl) ([]source, error) {
	var out []source
	wantUp := o.service == "" || inProcess[o.service]
	if wantUp {
		if _, err := os.Stat(upLogPath(cfg)); err == nil {
			out = append(out, source{name: "cloudburrow", open: func(ctx context.Context) (io.ReadCloser, func() error, error) {
				return openUpLog(ctx, upLogPath(cfg), o)
			}})
		}
	}
	if o.service != "" && inProcess[o.service] {
		return out, nil
	}

	if o.service != string(config.ServiceRun) {
		pods, err := ownedPods(ctx, k, cfg.Cluster.Namespace, "cloudburrow.dev/owned=true")
		if err != nil {
			return nil, err
		}
		for _, p := range pods {
			if o.service != "" && p.app != o.service && !strings.HasPrefix(p.app, o.service+"-") {
				continue
			}
			out = append(out, podSources(k, p, o)...)
		}
	}
	if o.service == "" || o.service == string(config.ServiceRun) {
		services, err := ownedRunServices(ctx, k, cfg.Name)
		if err != nil {
			return nil, err
		}
		for runName, ksvc := range services {
			if o.resource != "" && runName != o.resource {
				continue
			}
			pods, err := ownedPods(ctx, k, runadapter.WorkloadNamespace, "serving.knative.dev/service="+ksvc)
			if err != nil {
				return nil, err
			}
			for _, p := range pods {
				p.source = "run/" + runName
				out = append(out, podSources(k, p, o)...)
			}
		}
		if o.resource != "" && len(services) > 0 {
			if _, ok := services[o.resource]; !ok {
				return nil, fmt.Errorf("no Cloud Run service %q in this instance", o.resource)
			}
		}
	}
	return out, nil
}

type pod struct {
	namespace, name, app, source string
	containers                   []string
}

func ownedPods(ctx context.Context, k *kubectl, namespace, selector string) ([]pod, error) {
	b, err := k.run(ctx, k.args("-n", namespace, "get", "pods", "-l", selector, "-o", "json"))
	if err != nil {
		return nil, fmt.Errorf("list pods in %s: %w", namespace, err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Containers     []struct{ Name string } `json:"containers"`
				InitContainers []struct{ Name string } `json:"initContainers"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("list pods in %s: %w", namespace, err)
	}
	var out []pod
	for _, it := range list.Items {
		p := pod{namespace: namespace, name: it.Metadata.Name, app: it.Metadata.Labels["app"], source: "kubernetes/" + it.Metadata.Labels["app"]}
		for _, c := range it.Spec.InitContainers {
			p.containers = append(p.containers, c.Name)
		}
		for _, c := range it.Spec.Containers {
			p.containers = append(p.containers, c.Name)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// ownedRunServices maps each of this instance's Cloud Run services to its
// Knative Service. Knative's pods carry no ownership label of CloudBurrow's,
// so ownership is checked on the Service, and only its pods are read.
func ownedRunServices(ctx context.Context, k *kubectl, instance string) (map[string]string, error) {
	b, err := k.run(ctx, k.args("-n", runadapter.WorkloadNamespace, "get", "ksvc",
		"-l", "cloudburrow.dev/owned=true,cloudburrow.dev/instance="+instance, "-o", "json"))
	if err != nil {
		// Knative is installed only when Cloud Run is enabled; without it
		// there are simply no services.
		return map[string]string{}, nil
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, it := range list.Items {
		name := it.Metadata.Annotations["cloudburrow.dev/cloud-run-name"]
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		if name == "" {
			name = it.Metadata.Name
		}
		out[name] = it.Metadata.Name
	}
	return out, nil
}

func podSources(k *kubectl, p pod, o logsOptions) []source {
	var out []source
	for _, c := range p.containers {
		c := c
		args := k.args("-n", p.namespace, "logs", p.name, "-c", c, "--timestamps", "--tail", strconv.Itoa(o.tail))
		if o.since > 0 {
			args = append(args, "--since", o.since.String())
		}
		if o.follow {
			args = append(args, "--follow")
		}
		out = append(out, source{name: p.source, resource: p.name + "/" + c,
			open: func(ctx context.Context) (io.ReadCloser, func() error, error) { return k.stream(ctx, args) }})
	}
	return out
}

// openUpLog reads the last lines of up.log, then, when following, what is
// appended to it until ctx ends.
func openUpLog(ctx context.Context, path string, o logsOptions) (io.ReadCloser, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	var tail []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		tail = append(tail, sc.Text())
		if o.tail >= 0 && len(tail) > o.tail {
			tail = tail[1:]
		}
	}
	pr, pw := io.Pipe()
	go func() {
		defer f.Close()
		for _, l := range tail {
			if _, err := io.WriteString(pw, l+"\n"); err != nil {
				return
			}
		}
		if !o.follow {
			_ = pw.Close()
			return
		}
		r := bufio.NewReader(f)
		var pending string
		for {
			chunk, err := r.ReadString('\n')
			pending += chunk
			if err == nil {
				// A whole line: `up` may write one in several pieces, so a
				// piece without its newline waits in pending rather than
				// being passed on, or lost, half-written.
				if _, werr := io.WriteString(pw, pending); werr != nil {
					return
				}
				pending = ""
				continue
			}
			select {
			case <-ctx.Done():
				_ = pw.Close()
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
	}()
	return pr, func() error { return nil }, nil
}
