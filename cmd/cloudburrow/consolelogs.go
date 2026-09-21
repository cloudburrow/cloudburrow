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

	"github.com/identity-wael/cloudburrow/internal/console"
	"github.com/identity-wael/cloudburrow/internal/lifecycle"
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
		if _, already := c.watching[pod.key()]; already {
			continue
		}
		podCtx, cancel := context.WithCancel(ctx)
		c.watching[pod.key()] = cancel
		go c.follow(podCtx, pod)
	}
}

type podRef struct {
	namespace, name, service string
}

func (p podRef) key() string { return p.namespace + "/" + p.name }

// source names the log's origin the way the console shows it.
func (p podRef) source() string {
	if p.service != "" {
		return "run/" + p.service
	}
	return "kubernetes/" + p.name
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
		pods = append(pods, podRef{
			namespace: item.Metadata.Namespace,
			name:      item.Metadata.Name,
			service:   item.Metadata.Labels["serving.knative.dev/service"],
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
		// Only the tail: a pod that has been running for an hour would
		// otherwise flood the buffer with history nobody asked for and push
		// out what is happening now.
		"--tail", "20",
	}
	// Knative pods have a sidecar; the user's container is the one worth
	// showing, and asking for it by name avoids a "choose a container" error.
	if pod.service != "" {
		args = append(args, "-c", "user-container")
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
			Resource:  pod.name,
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
