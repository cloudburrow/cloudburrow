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

// Unavailable is a reason the terminal cannot be used that the user can do
// something about, as opposed to a failure of this package.
type Unavailable struct{ Reason string }

func (u *Unavailable) Error() string { return u.Reason }

// pullFailures are the waiting reasons that will not resolve by waiting.
var pullFailures = map[string]bool{
	"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true,
	"ErrImageNeverPull": true, "CreateContainerConfigError": true, "CreateContainerError": true,
	"CrashLoopBackOff": true,
}

// Prepare makes sure the terminal pod exists, matches this instance and is
// ready, reporting what it is waiting for through progress. The first use
// pulls the image, which is large; ctx bounds the wait.
func (m *Manager) Prepare(ctx context.Context, progress func(string)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if progress == nil {
		progress = func(string) {}
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

	last := ""
	for {
		pod, err := m.readPod(ctx)
		if err != nil {
			return fmt.Errorf("read the terminal pod: %w", err)
		}
		if pod == nil {
			return &Unavailable{Reason: "the terminal pod was deleted while it was starting; open the terminal again to recreate it"}
		}
		msg, ready, failure := describe(pod)
		if failure != "" {
			return &Unavailable{Reason: failure}
		}
		if ready {
			return nil
		}
		if msg != last {
			progress(msg)
			last = msg
		}
		select {
		case <-ctx.Done():
			return &Unavailable{Reason: "the terminal pod did not become ready in time: " + last}
		case <-time.After(m.cfg.Poll):
		}
	}
}

// describe says what a starting pod is waiting for, whether it is ready,
// and why it will not start when it will not.
func describe(p *podState) (msg string, ready bool, failure string) {
	for _, c := range p.Status.ContainerStatuses {
		if c.Ready {
			return "", true, ""
		}
		if w := c.State.Waiting; w != nil {
			if pullFailures[w.Reason] {
				detail := w.Reason
				if w.Message != "" {
					detail += ": " + w.Message
				}
				return "", false, "the terminal pod cannot start (" + detail + "). Its image is pulled from " +
					"gcr.io by digest on first use, so the cluster needs to reach that registry once"
			}
			if w.Reason == "ContainerCreating" {
				return "Pulling the terminal image (the Cloud SDK with kubectl, about 1 GB compressed; first use only)", false, ""
			}
			return "Waiting for the terminal container: " + w.Reason, false, ""
		}
		if t := c.State.Terminated; t != nil {
			return "", false, "the terminal container exited (" + t.Reason + ")"
		}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == "PodScheduled" && c.Status == "False" {
			return "", false, "the terminal pod cannot be scheduled: " + c.Message
		}
	}
	return "Waiting for the terminal pod to be scheduled", false, ""
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
