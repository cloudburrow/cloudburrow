// Package terminal runs the console's Cloud Shell-style terminal (#781): a
// pod in the instance's own cluster, from a pinned Cloud SDK image, and an
// interactive `kubectl exec` into it.
//
// The shell is never on the developer's machine. A console page that could
// run host commands would be remote code execution for any page that got past
// the console's origin checks; a pod in the cluster can reach what any other
// pod can, and nothing of the host's. The pod is given the instance's pod
// environment and gcloud configuration, and no credential: gcloud's
// auth/disable_credentials is set, and no host path is mounted.
//
// Only this package knows what the pod looks like. It reaches the cluster
// through the one kubectl runner in internal/k8s (docs/architecture.md §3,
// rule 2), and the console reaches it through a narrow interface of its own.
package terminal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// Image is the terminal pod's image: Google's Cloud SDK image at the gcloud
// release the compat suite pins (dependencies.json, consoleTerminalImage).
//
// The full image rather than :stable or :slim, because it is the only one
// Google publishes with kubectl in it (586.0.0-stable and -slim were
// inspected and have none). It carries no Terraform; neither does any
// Cloud SDK image.
const Image = "gcr.io/google.com/cloudsdktool/google-cloud-cli@sha256:2b156513775d9ffdba9be27f47ec57567573d10ca77df1eed9f711b9fbf3c9ce"

// Names of the objects this package creates in the instance's namespace.
const (
	PodName        = "cloudburrow-terminal"
	ServiceAccount = "cloudburrow-terminal"
	container      = "shell"
	// ServiceLabelValue is the cloudburrow.dev/service label the objects
	// carry.
	ServiceLabelValue = "terminal"
	// specAnnotation records the hash of the pod spec, so a pod made by an
	// instance with other endpoints is replaced rather than reused.
	specAnnotation = "cloudburrow.dev/terminal-spec"
	rcPath         = "/etc/cloudburrow.bashrc"
	stateDir       = "/var/run/cloudburrow"
)

// Config is what the manager needs from the instance.
type Config struct {
	// Kube is the runner for the instance's namespace, where the pod lives.
	Kube *k8s.Runner
	// KubeIn returns a runner for another namespace, for the RoleBindings
	// that let kubectl in the pod read it. Nil means only Kube's namespace.
	KubeIn func(namespace string) *k8s.Runner
	// Instance is the instance's name, for the instance label.
	Instance string
	// ViewNamespaces are the namespaces kubectl in the pod may read, with
	// Kubernetes' built-in view role: the instance's and the workloads'.
	ViewNamespaces []string
	// Env is the pod's environment, read when the pod is made: the
	// instance's pod variables and its gcloud configuration. It must hold no
	// credential.
	Env func() map[string]string
	// Image overrides Image, for tests.
	Image string
	// Poll is how often Prepare reads the pod while it starts; zero means
	// two seconds.
	Poll time.Duration
	// Stall is how long Prepare waits on a pod that shows no change while
	// no image pull is under way; zero means ten minutes. A pull under way
	// is waited on for as long as the kubelet reports it (#824).
	Stall time.Duration
	// Before, when set, runs first: `up`'s background import of the image
	// from the offline cache, which the pod waits for rather than racing
	// it with a pull.
	Before func(ctx context.Context, progress func(string)) error
	// Now is the clock, for tests; nil means time.Now.
	Now func() time.Time
}

// Manager makes the terminal pod and opens shells in it.
type Manager struct {
	cfg Config
	mu  sync.Mutex
}

// New returns a manager.
func New(cfg Config) *Manager {
	if cfg.Image == "" {
		cfg.Image = Image
	}
	if cfg.Poll == 0 {
		cfg.Poll = 2 * time.Second
	}
	if cfg.Stall == 0 {
		cfg.Stall = 10 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Manager{cfg: cfg}
}

// rc is the shell's startup file. It keeps the image's own bash setup, then
// makes the prompt name the project and follow the console's toolbar: the
// console writes the selected project to a file per session, and each
// prompt picks it up, so a switch applies at the next prompt without typing
// anything into the shell for the user.
const rc = `[ -f /etc/bash.bashrc ] && . /etc/bash.bashrc
[ -f "$HOME/.bashrc" ] && . "$HOME/.bashrc"
__cloudburrow_project() {
  local f="` + stateDir + `/project-$CLOUDBURROW_SESSION" p
  [ -r "$f" ] || return 0
  p="$(cat "$f")"
  if [ -z "$p" ]; then unset CLOUDSDK_CORE_PROJECT; else export CLOUDSDK_CORE_PROJECT="$p"; fi
}
PROMPT_COMMAND="__cloudburrow_project${PROMPT_COMMAND:+;$PROMPT_COMMAND}"
PS1='\[\e[1;32m\]cloudburrow\[\e[0m\] (\[\e[1;33m\]${CLOUDSDK_CORE_PROJECT:-no project}\[\e[0m\]):\w\$ '
`

type object = map[string]any

func (m *Manager) labels() object {
	return object{
		k8s.OwnedLabel:    k8s.OwnedValue,
		k8s.InstanceLabel: m.cfg.Instance,
		k8s.ServiceLabel:  ServiceLabelValue,
	}
}

// podSpec is the pod's spec, which is also what its hash is taken over.
func (m *Manager) podSpec() object {
	env := map[string]string{}
	if m.cfg.Env != nil {
		for k, v := range m.cfg.Env() {
			env[k] = v
		}
	}
	env["CLOUDBURROW_BASHRC"] = rc
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	vars := make([]object, 0, len(names))
	for _, k := range names {
		vars = append(vars, object{"name": k, "value": env[k]})
	}
	return object{
		"serviceAccountName":            ServiceAccount,
		"terminationGracePeriodSeconds": 1,
		"enableServiceLinks":            false,
		"containers": []object{{
			"name":            container,
			"image":           m.cfg.Image,
			"imagePullPolicy": "IfNotPresent",
			// The startup file is written from the environment, so the pod
			// needs no ConfigMap; the container then idles until a shell is
			// opened in it.
			"command": []string{"bash", "-c",
				`mkdir -p ` + stateDir + ` && printf '%s' "$CLOUDBURROW_BASHRC" > ` + rcPath + ` && exec sleep infinity`},
			"workingDir": "/root",
			"env":        vars,
		}},
	}
}

func specHash(spec object) string {
	b, _ := json.Marshal(spec)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// manifest is the ServiceAccount and the pod, as one List for apply.
func (m *Manager) manifest() (string, string) {
	spec := m.podSpec()
	hash := specHash(spec)
	list := object{
		"apiVersion": "v1",
		"kind":       "List",
		"items": []object{
			{
				"apiVersion": "v1",
				"kind":       "ServiceAccount",
				"metadata":   object{"name": ServiceAccount, "labels": m.labels()},
			},
			{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata": object{
					"name":        PodName,
					"labels":      m.labels(),
					"annotations": object{specAnnotation: hash},
				},
				"spec": spec,
			},
		},
	}
	b, _ := json.Marshal(list)
	return string(b), hash
}

// roleBinding lets the pod's service account read one namespace.
func (m *Manager) roleBinding(namespace string) string {
	b, _ := json.Marshal(object{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "RoleBinding",
		"metadata":   object{"name": ServiceAccount + "-view", "namespace": namespace, "labels": m.labels()},
		"roleRef":    object{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "view"},
		"subjects": []object{{
			"kind": "ServiceAccount", "name": ServiceAccount, "namespace": m.cfg.Kube.Namespace(),
		}},
	})
	return string(b)
}

// podState is the part of the pod Prepare reads.
type podState struct {
	Metadata struct {
		UID               string            `json:"uid"`
		Annotations       map[string]string `json:"annotations"`
		DeletionTimestamp string            `json:"deletionTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
		ContainerStatuses []struct {
			Ready bool `json:"ready"`
			State struct {
				Waiting *struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"waiting"`
				Terminated *struct {
					Reason string `json:"reason"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

func (m *Manager) readPod(ctx context.Context) (*podState, error) {
	out, err := m.cfg.Kube.Get(ctx, "pod", PodName, "json")
	if k8s.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p podState
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		return nil, fmt.Errorf("read the terminal pod: %w", err)
	}
	return &p, nil
}

// pullState is what the kubelet's events say of the image pull for one
// pod (#824). The kubelet reports a pull's start ("Pulling"), its end
// ("Pulled", with the image's size on recent kubelets) and its failure
// ("Failed"), and nothing in between: no byte count is observable through
// the API while a pull is under way.
type pullState struct {
	// Active is a pull the kubelet has started and not finished.
	Active bool
	// Since is when the kubelet started the pull under way, by its clock.
	Since time.Time
	// Pulled is a finished pull, or an image already on the node.
	Pulled bool
	// Present is an image that was already on the node.
	Present bool
	// Bytes is the image's size as the Pulled event gives it; zero when it
	// does not.
	Bytes int64
	// Failed is the message of the latest failed pull.
	Failed string
}

// eventList is the part of `kubectl get events -o json` pullState reads.
type eventList struct {
	Items []struct {
		InvolvedObject struct {
			UID string `json:"uid"`
		} `json:"involvedObject"`
		Reason         string    `json:"reason"`
		Message        string    `json:"message"`
		FirstTimestamp time.Time `json:"firstTimestamp"`
		LastTimestamp  time.Time `json:"lastTimestamp"`
		EventTime      time.Time `json:"eventTime"`
		Series         *struct {
			LastObservedTime time.Time `json:"lastObservedTime"`
		} `json:"series"`
	} `json:"items"`
}

var imageSizeRE = regexp.MustCompile(`Image size: ([0-9]+) bytes`)

// parsePull reads the pull's state from a pod's events. Events of an
// earlier pod of the same name are left out by uid.
func parsePull(out, uid string) (pullState, bool) {
	var list eventList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return pullState{}, false
	}
	var st pullState
	var pulling, pulled, failed time.Time
	for _, e := range list.Items {
		if uid != "" && e.InvolvedObject.UID != "" && e.InvolvedObject.UID != uid {
			continue
		}
		at := e.LastTimestamp
		if e.Series != nil && e.Series.LastObservedTime.After(at) {
			at = e.Series.LastObservedTime
		}
		if at.IsZero() {
			at = e.EventTime
		}
		if at.IsZero() {
			at = e.FirstTimestamp
		}
		switch e.Reason {
		case "Pulling":
			if !at.Before(pulling) {
				pulling = at
			}
		case "Pulled":
			if !at.Before(pulled) {
				pulled = at
				st.Present = strings.Contains(e.Message, "already present")
				st.Bytes = 0
				if m := imageSizeRE.FindStringSubmatch(e.Message); m != nil {
					st.Bytes, _ = strconv.ParseInt(m[1], 10, 64)
				}
			}
		case "Failed":
			if strings.Contains(e.Message, "image") && !at.Before(failed) {
				failed = at
				st.Failed = e.Message
			}
		}
	}
	st.Pulled = !pulled.IsZero() && !pulled.Before(pulling)
	st.Active = !pulling.IsZero() && pulling.After(pulled) && !failed.After(pulling)
	if st.Active {
		st.Since = pulling
	}
	if st.Pulled || st.Active {
		st.Failed = ""
	}
	return st, true
}

func (m *Manager) readPull(ctx context.Context, uid string) (pullState, bool) {
	out, err := m.cfg.Kube.Do(ctx, "", "get", "events",
		"--field-selector", "involvedObject.kind=Pod,involvedObject.name="+PodName, "-o", "json")
	if err != nil {
		return pullState{}, false
	}
	return parsePull(out, uid)
}

// Unavailable is a reason the terminal cannot be used that the user can do
// something about, as opposed to a failure of this package.
type Unavailable struct{ Reason string }

func (u *Unavailable) Error() string { return u.Reason }

// pullFailures are the waiting reasons of an image that cannot be pulled.
// Each is a real failure: the kubelet has given up, for now, on the pull.
var pullFailures = map[string]bool{
	"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true, "ErrImageNeverPull": true,
}

// startFailures are the other waiting reasons that will not resolve by
// waiting.
var startFailures = map[string]bool{
	"CreateContainerConfigError": true, "CreateContainerError": true, "CrashLoopBackOff": true,
}

// readRetries is how many reads of the pod in a row may fail before
// Prepare gives up: one failed kubectl call is not a failed terminal.
const readRetries = 5

// Prepare makes sure the terminal pod exists, matches this instance and is
// ready, reporting what it is waiting for through progress.
//
// The first use pulls the image, about 1 GB compressed, which can take a
// quarter of an hour or more on a slow link. Prepare waits for as long as
// the kubelet reports the pull under way, bounded only by ctx; it gives up
// on a real failure (an image that cannot be pulled, a pod that cannot be
// scheduled or has gone) with the cluster's reason, and on a pod that
// shows no change for Stall while nothing is being pulled.
func (m *Manager) Prepare(ctx context.Context, progress func(string)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if progress == nil {
		progress = func(string) {}
	}
	if m.cfg.Before != nil {
		if err := m.cfg.Before(ctx, progress); err != nil {
			return err
		}
	}
	manifest, hash := m.manifest()

	pod, err := m.readPod(ctx)
	if err != nil {
		return fmt.Errorf("read the terminal pod: %w", err)
	}
	stale := pod != nil && (pod.Metadata.Annotations[specAnnotation] != hash ||
		pod.Status.Phase == "Failed" || pod.Status.Phase == "Succeeded" || pod.Metadata.DeletionTimestamp != "")
	if stale {
		// A pod's spec cannot be changed in place; one made for other
		// endpoints, or one that has stopped, is replaced.
		progress("Replacing the terminal pod, which was made for a different configuration or has stopped")
		if err := m.cfg.Kube.Delete(ctx, "pod", PodName, true); err != nil {
			return fmt.Errorf("replace the terminal pod: %w", err)
		}
		if _, err := m.cfg.Kube.Do(ctx, "", "wait", "--for=delete", "pod/"+PodName, "--timeout=60s"); err != nil && !k8s.IsNotFound(err) {
			return fmt.Errorf("wait for the old terminal pod to go: %w", err)
		}
		pod = nil
	}
	if pod == nil {
		progress("Creating the terminal pod")
		if err := m.cfg.Kube.Apply(ctx, manifest, k8s.ApplyOptions{}); err != nil {
			return fmt.Errorf("create the terminal pod: %w", err)
		}
		for _, ns := range m.cfg.ViewNamespaces {
			kube := m.cfg.Kube
			if ns != kube.Namespace() {
				if m.cfg.KubeIn == nil {
					continue
				}
				kube = m.cfg.KubeIn(ns)
			}
			if err := kube.Apply(ctx, m.roleBinding(ns), k8s.ApplyOptions{}); err != nil && !k8s.IsNotFound(err) {
				return fmt.Errorf("let the terminal read namespace %s: %w", ns, err)
			}
		}
	}

	lastMsg, lastKey := "", ""
	changed := m.cfg.Now()
	failedReads := 0
	for {
		pod, err := m.readPod(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			failedReads++
			if failedReads >= readRetries {
				return fmt.Errorf("read the terminal pod: %w", err)
			}
		case err == nil && pod == nil:
			return &Unavailable{Reason: "the terminal pod was deleted while it was starting; open the terminal again to recreate it"}
		case err == nil:
			failedReads = 0
			var pull pullState
			if needsPull(pod) {
				pull, _ = m.readPull(ctx, pod.Metadata.UID)
			}
			now := m.cfg.Now()
			st := describe(pod, pull, now)
			if st.failure != "" {
				return &Unavailable{Reason: st.failure}
			}
			if st.ready {
				return nil
			}
			if st.key != lastKey {
				lastKey, changed = st.key, now
			} else if !st.pulling && now.Sub(changed) >= m.cfg.Stall {
				return &Unavailable{Reason: "the terminal pod has shown no progress for " +
					m.cfg.Stall.String() + ": " + st.msg}
			}
			if st.msg != lastMsg {
				progress(st.msg)
				lastMsg = st.msg
			}
		}
		select {
		case <-ctx.Done():
			return &Unavailable{Reason: "stopped waiting for the terminal pod: " + lastMsg}
		case <-time.After(m.cfg.Poll):
		}
	}
}

// needsPull reports whether the pod's events are worth reading: while its
// container is being created or its image cannot be pulled.
func needsPull(p *podState) bool {
	if len(p.Status.ContainerStatuses) == 0 {
		return true
	}
	for _, c := range p.Status.ContainerStatuses {
		if w := c.State.Waiting; w != nil && (w.Reason == "ContainerCreating" || pullFailures[w.Reason]) {
			return true
		}
	}
	return false
}

// startState is what a starting pod is waiting for.
type startState struct {
	ready bool
	// failure is why the pod will not start, when it will not.
	failure string
	// msg is what the drawer shows.
	msg string
	// key is what is being waited for; it changes when the wait moves on,
	// not with the elapsed time msg carries.
	key string
	// pulling is an image pull under way, which is waited on without
	// bound.
	pulling bool
}

// imageNote describes the image while it is pulled. The size is
// dependencies.json's measurement of the pinned image
// (consoleTerminal.cloudSdkImage.measured).
const imageNote = "the Cloud SDK with kubectl, about 1 GB compressed; first use only"

// describe says what a starting pod is waiting for, whether it is ready,
// and why it will not start when it will not.
func describe(p *podState, pull pullState, now time.Time) startState {
	for _, c := range p.Status.Conditions {
		if c.Type == "PodScheduled" && c.Status == "False" {
			detail := c.Reason
			if c.Message != "" {
				detail += ": " + c.Message
			}
			return startState{failure: "the terminal pod cannot be scheduled (" + detail + ")"}
		}
	}
	for _, c := range p.Status.ContainerStatuses {
		if c.Ready {
			return startState{ready: true}
		}
		if w := c.State.Waiting; w != nil {
			detail := w.Reason
			if w.Message != "" {
				detail += ": " + w.Message
			}
			if pullFailures[w.Reason] {
				if pull.Failed != "" && !strings.Contains(detail, pull.Failed) {
					detail += "; the last pull failed with: " + pull.Failed
				}
				return startState{failure: "the terminal image cannot be pulled (" + detail + "). It is pulled from " +
					"gcr.io by digest on first use, so the cluster needs to reach that registry once; " +
					"`cloudburrow prefetch` stores it for `up --offline`"}
			}
			if startFailures[w.Reason] {
				return startState{failure: "the terminal pod cannot start (" + detail + ")"}
			}
			if w.Reason == "ContainerCreating" {
				return creating(pull, now)
			}
			return startState{key: "waiting:" + w.Reason, msg: "Waiting for the terminal container: " + w.Reason}
		}
		if t := c.State.Terminated; t != nil {
			return startState{failure: "the terminal container exited (" + t.Reason + ")"}
		}
	}
	if len(p.Status.ContainerStatuses) == 0 && (pull.Active || pull.Pulled) {
		return creating(pull, now)
	}
	return startState{key: "scheduling", msg: "Waiting for the terminal pod to be scheduled"}
}

// creating describes a container being created from what the pull events
// say. Only numbers the cluster reports are shown: the time since the
// kubelet started the pull, and the image's size once it has finished.
func creating(pull pullState, now time.Time) startState {
	switch {
	case pull.Active:
		elapsed := now.Sub(pull.Since)
		if elapsed < 0 {
			elapsed = 0
		}
		return startState{key: "pulling", pulling: true,
			msg: "Pulling the terminal image (" + imageNote + "): still pulling, " + roughly(elapsed) +
				" so far. The cluster reports no byte count until the pull finishes"}
	case pull.Present:
		return startState{key: "pulled", msg: "The terminal image is on the node; starting the terminal container"}
	case pull.Pulled && pull.Bytes > 0:
		return startState{key: "pulled", msg: "Pulled the terminal image (" + size(pull.Bytes) +
			", as the kubelet reports it); starting the terminal container"}
	case pull.Pulled:
		return startState{key: "pulled", msg: "Pulled the terminal image; starting the terminal container"}
	}
	return startState{key: "creating", msg: "Starting the terminal container (" + imageNote + ", is pulled before it starts)"}
}

// roughly is an elapsed time to ten seconds, so the message changes, and a
// screen reader announces it, no more often than that.
func roughly(d time.Duration) string {
	return d.Truncate(10 * time.Second).String()
}

// size is a byte count in the units the kubelet's number supports.
func size(b int64) string {
	const mb = 1000 * 1000
	if b < mb {
		return strconv.FormatInt(b, 10) + " bytes"
	}
	return strconv.FormatFloat(float64(b)/mb, 'f', 0, 64) + " MB"
}

// projectRE is what SetProject and Open accept: a project ID, or empty for
// none. It is passed as an argument, never interpolated into a command, but a
// value that could not be a project is refused all the same.
var projectRE = regexp.MustCompile(`^([a-z][a-z0-9-]{0,62})?$`)

// Session is one interactive shell in the terminal pod.
type Session struct {
	k8s.TTY
	id   string
	kube *k8s.Runner
}

// Open starts an interactive bash in the pod, on a terminal of cols by rows,
// with CLOUDSDK_CORE_PROJECT set to project (none when it is empty). Prepare
// must have succeeded.
func (m *Manager) Open(project string, cols, rows uint16) (*Session, error) {
	if !projectRE.MatchString(project) {
		return nil, fmt.Errorf("%q is not a project ID", project)
	}
	id, err := sessionID()
	if err != nil {
		return nil, err
	}
	env := []string{"env", "CLOUDBURROW_SESSION=" + id, "TERM=xterm-256color"}
	if project != "" {
		env = append(env, "CLOUDSDK_CORE_PROJECT="+project)
	}
	args := append([]string{"exec", "-i", "-t", PodName, "-c", container, "--"}, env...)
	args = append(args, "bash", "--rcfile", rcPath, "-i")
	tty, err := m.cfg.Kube.Terminal(cols, rows, args...)
	if err != nil {
		return nil, fmt.Errorf("attach to the terminal pod: %w", err)
	}
	return &Session{TTY: tty, id: id, kube: m.cfg.Kube}, nil
}

// SetProject makes the session's CLOUDSDK_CORE_PROJECT and prompt follow a
// newly selected project from its next prompt. It writes the session's
// project file through a separate exec rather than typing into the shell,
// which would land in whatever program the user has open.
func (s *Session) SetProject(ctx context.Context, project string) error {
	if !projectRE.MatchString(project) {
		return fmt.Errorf("%q is not a project ID", project)
	}
	_, err := s.kube.Do(ctx, "", "exec", PodName, "-c", container, "--",
		"sh", "-c", `printf '%s' "$1" > "`+stateDir+`/project-$2"`, "sh", project, s.id)
	if err != nil {
		return fmt.Errorf("switch the terminal's project: %w", err)
	}
	return nil
}

func sessionID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// IsUnavailable reports whether err is a reason the terminal cannot be used,
// which the console shows as it stands.
func IsUnavailable(err error) (string, bool) {
	var u *Unavailable
	if errors.As(err, &u) {
		return u.Reason, true
	}
	return "", false
}
