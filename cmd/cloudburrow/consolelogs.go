package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
)

// logCollector streams pod logs from the cluster into the console recorder.
//
// It follows pods rather than reading them once: a log view that only shows
// what existed when the page loaded is a log view that never shows the line
// explaining the failure that just happened.
// followedNamespaces are the namespaces whose pods are followed.
//
// Deliberately not every namespace. The Kubernetes control plane, kourier and
// Knative's own components produce a continuous stream of readiness probes
// and leader-election chatter that, measured on a running instance, was 90%
// of the buffer — it pushes out the handful of lines that actually explain a
// developer's failure. A log view dominated by kube-proxy is a log view
// nobody reads.
//
// The infrastructure is still visible: the Events screen reports what the
// cluster is doing, and `kubectl logs` is always available for a specific
// component.
var followedNamespaces = []string{"default", "cloudburrow"}

type logCollector struct {
	kubeconfig string
	namespaces []string
	recorder   *console.Recorder

	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	watching map[string]context.CancelFunc
}

func newLogCollector(kubeconfig string, namespaces []string, recorder *console.Recorder) *logCollector {
	if kubeconfig == "" || recorder == nil {
		return nil
	}
	if len(namespaces) == 0 {
		namespaces = followedNamespaces
	}
	return &logCollector{
		kubeconfig: kubeconfig, namespaces: namespaces, recorder: recorder,
		watching: map[string]context.CancelFunc{},
	}
}

func (c *logCollector) Name() string { return "console-logs" }

func (c *logCollector) register(coord *lifecycle.Coordinator) {
	if c == nil {
		return
	}
	coord.Register(c)
}

// Start begins following pods. It does not block.
func (c *logCollector) Start(ctx context.Context) error {
	if c == nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})

	c.mu.Lock()
	c.cancel, c.done = cancel, done
	c.mu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			c.sweep(runCtx)
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return nil
}

// sweep starts following any pod that is not already being followed.
func (c *logCollector) sweep(ctx context.Context) {
	pods, err := c.listPods(ctx)
	if err != nil {
		// A cluster that cannot be listed is not an error worth logging on
		// every tick; it is reported by the screens that need it.
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, pod := range pods {
		// One follower per container, not per pod.
		//
		// `kubectl logs <pod>` with no -c fails on any pod with more than one
		// container, so every multi-container pod in the cluster produced
		// nothing at all — and the Knative case worked only because it asked
		// for user-container by name, which threw away the queue-proxy's lines.
		// Those carry the cold-start and routing errors, which is exactly what
		// someone is looking for when a Cloud Run service is not answering.
		for _, container := range pod.containers {
			ref := pod
			ref.container = container
			if _, already := c.watching[ref.key()]; already {
				continue
			}
			podCtx, cancel := context.WithCancel(ctx)
			c.watching[ref.key()] = cancel
			go c.follow(podCtx, ref)
		}
	}
}

type podRef struct {
	namespace, name, service string
	// containers are the pod's containers, including init containers: an init
	// container that fails is the reason the pod never started, and its output
	// is the only account of why.
	containers []string
	// container is the one this follower is reading, set when a ref is narrowed
	// from a pod to one of its containers.
	container string
}

func (p podRef) key() string { return p.namespace + "/" + p.name + "/" + p.container }

// source names the log's origin the way the console shows it.
func (p podRef) source() string {
	if p.service != "" {
		return "run/" + p.service
	}
	return "kubernetes/" + p.name
}

// resource names which container produced a line.
//
// The container is part of it, because a pod's own name cannot distinguish the
// application's output from its sidecar's — and on a Knative pod those say very
// different things about what is wrong.
func (p podRef) resource() string {
	if p.container == "" {
		return p.name
	}
	return p.name + "/" + p.container
}

func (c *logCollector) listPods(ctx context.Context) ([]podRef, error) {
	var all []podRef
	for _, ns := range c.namespaces {
		pods, err := c.listPodsIn(ctx, ns)
		if err != nil {
			// One missing namespace must not stop the others: `default`
			// exists before `cloudburrow` does.
			continue
		}
		all = append(all, pods...)
	}
	return all, nil
}

func (c *logCollector) listPodsIn(ctx context.Context, namespace string) ([]podRef, error) {
	args := []string{"--kubeconfig", c.kubeconfig, "-n", namespace, "get", "pods", "-o", "json"}
	out, err := exec.CommandContext(ctx, "kubectl", args...).Output()
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name      string            `json:"name"`
				Namespace string            `json:"namespace"`
				Labels    map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Containers     []struct{ Name string } `json:"containers"`
				InitContainers []struct{ Name string } `json:"initContainers"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}

	var pods []podRef
	for _, item := range list.Items {
		if item.Status.Phase == "Succeeded" {
			continue
		}
		var containers []string
		// Init containers first, because that is the order they run in and the
		// order someone reads a failed startup in.
		for _, c := range item.Spec.InitContainers {
			containers = append(containers, c.Name)
		}
		for _, c := range item.Spec.Containers {
			containers = append(containers, c.Name)
		}
		if len(containers) == 0 {
			// A pod with no containers in its spec is not something to guess
			// about: following it with no -c would fail, and inventing a name
			// would fail differently.
			continue
		}
		pods = append(pods, podRef{
			namespace:  item.Metadata.Namespace,
			name:       item.Metadata.Name,
			service:    item.Metadata.Labels["serving.knative.dev/service"],
			containers: containers,
		})
	}
	return pods, nil
}

// follow streams one pod's logs until it ends.
func (c *logCollector) follow(ctx context.Context, pod podRef) {
	defer func() {
		c.mu.Lock()
		delete(c.watching, pod.key())
		c.mu.Unlock()
	}()

	args := []string{
		"--kubeconfig", c.kubeconfig, "-n", pod.namespace,
		"logs", pod.name, "--follow", "--timestamps",
		// Always by name. Without -c, kubectl refuses any pod with more than
		// one container — and it is also what makes each line attributable to
		// the container that wrote it.
		"-c", pod.container,
		// Only the tail: a pod that has been running for an hour would
		// otherwise flood the buffer with history nobody asked for and push
		// out what is happening now.
		"--tail", "20",
	}

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}

	scanner := bufio.NewScanner(stdout)
	// One line can be long; the recorder truncates, but the scanner must not
	// give up on the stream before it gets there.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		timestamp, message := splitTimestamp(line)
		c.recorder.Log(console.Entry{
			Timestamp: timestamp,
			Severity:  severityOf(message),
			Source:    pod.source(),
			Resource:  pod.resource(),
			Message:   message,
		})
	}
	_ = cmd.Wait()
}

// splitTimestamp separates kubectl's RFC3339 prefix from the line.
func splitTimestamp(line string) (time.Time, string) {
	stamp, rest, ok := strings.Cut(line, " ")
	if !ok {
		return time.Time{}, line
	}
	t, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return time.Time{}, line
	}
	return t.UTC(), rest
}

// severityOf infers a level from the line.
//
// Inferred, and labelled as such in the docs: container logs carry no
// structured severity, so this is a heuristic over the words applications
// conventionally use. It is used for filtering, never to claim an
// application said something it did not.
func severityOf(message string) console.Severity {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "error") || strings.Contains(lower, "panic") ||
		strings.Contains(lower, "fatal") || strings.Contains(lower, "failed"):
		return console.SeverityError
	case strings.Contains(lower, "warn"):
		return console.SeverityWarning
	default:
		return console.SeverityInfo
	}
}

func (c *logCollector) Stop(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.cancel = nil
	c.mu.Unlock()

	if cancel == nil {
		return nil
	}
	cancel()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	return nil
}

// taskLogger feeds Cloud Tasks dispatch attempts into the console.
//
// Attempts are the thing a developer actually needs when a task is not
// arriving: the queue says "1 task", and only the attempt says why it is
// still there.
type taskLogger struct {
	recorder *console.Recorder
}

func (t taskLogger) Attempt(queue, task string, attempt int, status int, err error) {
	if t.recorder == nil {
		return
	}
	severity := console.SeverityInfo
	message := fmt.Sprintf("attempt %d of %s returned %d", attempt, task, status)
	if err != nil {
		severity = console.SeverityError
		message = fmt.Sprintf("attempt %d of %s failed: %v", attempt, task, err)
	} else if status < 200 || status >= 300 {
		severity = console.SeverityWarning
	}
	t.recorder.Log(console.Entry{
		Severity: severity, Source: "tasks", Resource: queue, Message: message,
	})
}
