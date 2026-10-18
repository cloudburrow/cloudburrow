package components

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// ErrInstallFailed means a component could not be installed or did not become
// ready within its bound.
var ErrInstallFailed = errors.New("component install failed")

// Installer applies CloudBurrow's components to a cluster.
type Installer struct {
	Kubeconfig string
	Namespace  string
	Instance   string
	// Kube runs kubectl, through internal/k8s; nil is the kubectl on PATH.
	Kube k8s.Invoker
	Out  io.Writer
	// Fetch downloads a pinned manifest; nil is an HTTPS GET. Manifests
	// overrides KnativeManifests. Both are for tests.
	Fetch     func(ctx context.Context, url string) ([]byte, error)
	Manifests []Manifest
}

// maxManifestBytes bounds a downloaded release YAML; serving-core is about
// 500 KB.
const maxManifestBytes = 16 << 20

// FetchManifest downloads a release YAML over HTTPS, bounded in time and
// size. It does not check the pin: the caller compares the bytes with
// Manifest.SHA256 before using them, as InstallKnative does.
func FetchManifest(ctx context.Context, url string) ([]byte, error) { return httpFetch(ctx, url) }

func httpFetch(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxManifestBytes {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", url, maxManifestBytes)
	}
	return b, nil
}

// kubectl runs kubectl --kubeconfig K ARGS, in kubectl's default context;
// a call that acts in a namespace names it in args.
func (i *Installer) kubectl(ctx context.Context, stdin string, args ...string) (string, error) {
	inv := i.Kube
	if inv == nil {
		inv = k8s.Subprocess{}
	}
	return k8s.NewWith(inv, i.Kubeconfig, "", "").Do(ctx, stdin, args...)
}

func (i *Installer) logf(format string, a ...any) {
	if i.Out != nil {
		fmt.Fprintf(i.Out, format, a...)
	}
}

// Apply applies a manifest from stdin.
func (i *Installer) Apply(ctx context.Context, manifest string) error {
	if _, err := i.kubectl(ctx, manifest, "apply", "-f", "-"); err != nil {
		return fmt.Errorf("%w: %w", ErrInstallFailed, err)
	}
	return nil
}

// EnsureNamespace creates the managed namespace, labelled for ownership.
//
// It runs whenever the cluster does, not only when a backend is installed:
// Cloud KMS keeps its state in this namespace and diagnose and logs look
// for it, and an instance whose services deploy no pod used to have none
// (#571). A second apply of the same manifest is a no-op.
func (i *Installer) EnsureNamespace(ctx context.Context) error {
	return i.Apply(ctx, NamespaceManifest(i.Namespace, i.Instance))
}

// InstallBackends creates the namespace and deploys the selected backends.
func (i *Installer) InstallBackends(ctx context.Context, backends []Backend, timeout time.Duration) error {
	if err := i.EnsureNamespace(ctx); err != nil {
		return err
	}
	for _, b := range backends {
		i.logf("  installing %s...\n", b.Name)
		if err := i.Apply(ctx, b.Manifest(i.Namespace, i.Instance)); err != nil {
			return fmt.Errorf("install %s: %w", b.Name, err)
		}
	}
	for _, b := range backends {
		if err := i.waitDeployment(ctx, i.Namespace, b.Name, timeout); err != nil {
			return err
		}
	}
	return nil
}

// waitDeployment waits for a Deployment's current spec to be rolled out.
//
// Readiness is asked of Kubernetes rather than assumed from a successful apply:
// an apply only means the object was accepted.
//
// Rollout status, not condition=Available. Available stays true through a
// rolling update while the old pod keeps serving, so a restart that changed
// the spec — `up --mode ephemeral` after a persistent run drops the volume —
// reported ready with the old pod, on the old volume, still answering (#297,
// measured). Rollout status returns only once every replica runs the new
// spec and the old ones are gone.
func (i *Installer) waitDeployment(ctx context.Context, namespace, name string, timeout time.Duration) error {
	_, err := i.kubectl(ctx, "", "-n", namespace, "rollout", "status",
		"deployment/"+name, "--watch=true",
		fmt.Sprintf("--timeout=%ds", int(timeout.Seconds())))
	if err != nil {
		// Surface why it never became ready, not just that it did not.
		events, _ := i.kubectl(ctx, "", "-n", namespace, "get", "events",
			"--field-selector", "involvedObject.name="+name, "--sort-by=.lastTimestamp")
		msg := fmt.Errorf("%w: %s did not become ready within %s: %w\n%s",
			ErrInstallFailed, name, timeout, err, truncate(events, 600))
		if logs := i.lastLogs(ctx, namespace, name); logs != "" {
			return fmt.Errorf("%w\n%s", msg, logs)
		}
		return msg
	}
	return nil
}

// lastLogs is the end of what the newest pod of a Deployment printed, for
// a rollout that failed. A container that exits at start is restarted
// forever, and the events say only that it is backing off; its own words,
// such as the storage server refusing its data directory (#780), are in
// its log. "" when there is no pod or no log.
func (i *Installer) lastLogs(ctx context.Context, namespace, name string) string {
	pods, err := i.kubectl(ctx, "", "-n", namespace, "get", "pods", "-l", "app="+name,
		"--sort-by=.metadata.creationTimestamp", "-o", "name")
	if err != nil {
		return ""
	}
	lines := strings.Fields(pods)
	if len(lines) == 0 {
		return ""
	}
	pod := lines[len(lines)-1]
	logs, err := i.kubectl(ctx, "", "-n", namespace, "logs", pod, "--tail=20")
	if err != nil || strings.TrimSpace(logs) == "" {
		return ""
	}
	return "last lines " + pod + " logged:\n" + truncate(logs, 2000)
}

// InstallKnative applies Knative Serving and its networking layer.
//
// Cloud Run workloads execute on Knative (ADR-0005); this is what makes the
// Cloud Run adapter possible at all.
func (i *Installer) InstallKnative(ctx context.Context, timeout time.Duration) error {
	manifests := i.Manifests
	if manifests == nil {
		manifests = KnativeManifests()
	}
	fetch := i.Fetch
	if fetch == nil {
		fetch = httpFetch
	}
	for _, m := range manifests {
		// Downloaded and checked here, then applied from stdin, so kubectl
		// never fetches bytes nobody compared with the pin (#597).
		body, err := fetch(ctx, m.URL)
		if err != nil {
			return fmt.Errorf("%w: download %s: %w", ErrInstallFailed, m.Name, err)
		}
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != m.SHA256 {
			return fmt.Errorf("%w: %s has sha256 %s, not the pinned %s; refusing to apply it",
				ErrInstallFailed, m.Name, got, m.SHA256)
		}
		i.logf("  verified %s sha256:%s\n", m.Name, m.SHA256[:12])
		i.logf("  applying %s\n", shortURL(m.URL))
		if _, err := i.kubectl(ctx, string(body), "apply", "-f", "-"); err != nil {
			return fmt.Errorf("%w: apply %s: %w", ErrInstallFailed, m.Name, err)
		}
		// The next manifest creates objects of the kinds this one defines,
		// and the API refuses them until the CRDs are Established. Applying
		// straight on failed intermittently with "no matches for kind" (#357).
		if strings.HasSuffix(m.Name, "-crds.yaml") {
			if _, err := i.kubectl(ctx, "", "wait", "--for=condition=Established", "crd",
				"-l", "knative.dev/crd-install=true",
				fmt.Sprintf("--timeout=%ds", int(timeout.Seconds()))); err != nil {
				return fmt.Errorf("%w: Knative CRDs from %s were not established: %w", ErrInstallFailed, m.Name, err)
			}
		}
	}

	// Kourier must be selected explicitly; Knative ships no default ingress.
	if err := i.patchValidated(ctx, "patch", "configmap/config-network",
		"-n", "knative-serving", "--type", "merge",
		"-p", `{"data":{"ingress-class":"kourier.ingress.networking.knative.dev"}}`); err != nil {
		return fmt.Errorf("%w: select kourier ingress: %w", ErrInstallFailed, err)
	}
	if err := i.ConfigureDeployment(ctx); err != nil {
		return err
	}

	// Publish the gateway and name services under a domain that resolves to
	// loopback without DNS egress (#86).
	if err := i.ConfigureIngress(ctx, DefaultDomain); err != nil {
		return err
	}

	return i.WaitKnative(ctx, timeout)
}

// WaitKnative waits for Knative Serving and its ingress to be Available.
//
// It is also the first step against a cluster that already has Knative:
// after stop and up its webhooks are still coming back, and a config patch
// made before they answer fails with "failed calling webhook" (#568,
// measured in a merge-queue build).
func (i *Installer) WaitKnative(ctx context.Context, timeout time.Duration) error {
	for _, ns := range []string{"knative-serving", "kourier-system"} {
		i.logf("  waiting for %s...\n", ns)
		if _, err := i.kubectl(ctx, "", "-n", ns, "wait",
			"--for=condition=Available", "deployment", "--all",
			fmt.Sprintf("--timeout=%ds", int(timeout.Seconds()))); err != nil {
			return fmt.Errorf("%w: %s did not become ready: %w", ErrInstallFailed, ns, err)
		}
	}
	return nil
}

// restartTarget is a Deployment, or with no name every Deployment of a
// namespace, that RestartKnative replaces.
type restartTarget struct {
	namespace string
	name      string
}

// warmRestarts are what a warm start replaces (#1610), in two stages.
//
// First CoreDNS and Knative Serving's own pods, which need only the API.
// CoreDNS fails as Knative does: its first list of the API, made before
// the Service network worked, failed, and its informers' retry backoff
// kept it unready for some 40 s.
//
// Then Kourier's gateway, once warmGate is Ready: it finds its
// configuration server, net-kourier-controller, by name. Started beside
// CoreDNS, its first lookups failed, its xDS stream's retry backoff held
// it unready, and the kubelet killed it for failing its liveness probe;
// up then took up to 58 s. Its readiness probe waits 10 s before its first
// try, so it starts as soon as what it needs is there, not after the
// activator.
var (
	warmRestarts = [][]restartTarget{
		{{namespace: "kube-system", name: "coredns"}, {namespace: "knative-serving"}},
		{{namespace: "kourier-system"}},
	}
	warmGate = []restartTarget{
		{namespace: "kube-system", name: "coredns"},
		{namespace: "knative-serving", name: "net-kourier-controller"},
	}
)

// RestartedAtAnnotation marks the pod template of a Deployment
// RestartKnative replaced, with the time it did (#1610).
const RestartedAtAnnotation = "cloudburrow.dev/restarted-at"

// knativeRestartEvery spaces RestartKnative's polls of the new pods.
var knativeRestartEvery = 250 * time.Millisecond

// RestartKnative replaces the pods of CoreDNS and of every Deployment of
// Knative Serving and its ingress (warmRestarts), and waits until each
// Deployment has as many Ready pods of its new template as it asks for
// (#1610).
//
// It is for a warm start, once the kubernetes Service answers: the pods the
// kubelet restarted with the node started before it did, and either exited
// and wait out a restart backoff (Knative) or wait out a retry backoff of
// their own (CoreDNS); and their Deployments' status predates the stop.
// The new pods start on a working network, and their readiness is this
// boot's. A rolling restart, not a delete, so a Deployment keeps the pods
// it has until the new ones are Ready; and the wait is for the new pods,
// not for the rollout, whose end waits for the old pods to terminate (the
// activator's grace period is 600 s).
func (i *Installer) RestartKnative(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	want := map[restartTarget]int{}
	for n, stage := range warmRestarts {
		if n > 0 {
			if err := i.waitRestarted(ctx, warmGate, want, stamp, deadline, timeout); err != nil {
				return err
			}
		}
		var restarted []string
		for _, t := range stage {
			names, err := i.restartTargets(ctx, t, stamp, want)
			if err != nil {
				return err
			}
			restarted = append(restarted, names...)
		}
		i.logf("  restarted %s on the running network\n", strings.Join(restarted, ", "))
	}
	all := make([]restartTarget, 0, len(want))
	for t := range want {
		all = append(all, t)
	}
	slices.SortFunc(all, func(a, b restartTarget) int {
		return strings.Compare(a.namespace+"/"+a.name, b.namespace+"/"+b.name)
	})
	i.logf("  waiting for the restarted pods...\n")
	return i.waitRestarted(ctx, all, want, stamp, deadline, timeout)
}

// restartTargets stamps the pod template of t's Deployments with stamp,
// records in want how many replicas each asks for, and returns their
// namespace/name.
func (i *Installer) restartTargets(ctx context.Context, t restartTarget, stamp string, want map[restartTarget]int) ([]string, error) {
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`, RestartedAtAnnotation, stamp)
	out, err := i.kubectl(ctx, "", "-n", t.namespace, "get", "deployment",
		"-o", `jsonpath={range .items[*]}{.metadata.name}={.spec.replicas}{"\n"}{end}`)
	if err != nil {
		return nil, fmt.Errorf("%w: list %s's deployments: %w", ErrInstallFailed, t.namespace, err)
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		name, replicas, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || name == "" || (t.name != "" && name != t.name) {
			continue
		}
		n, err := strconv.Atoi(replicas)
		if err != nil {
			n = 1
		}
		if _, err := i.kubectl(ctx, "", "-n", t.namespace, "patch", "deployment", name,
			"--type", "merge", "-p", patch); err != nil {
			return nil, fmt.Errorf("%w: restart %s/%s: %w", ErrInstallFailed, t.namespace, name, err)
		}
		want[restartTarget{namespace: t.namespace, name: name}] = n
		names = append(names, t.namespace+"/"+name)
	}
	return names, nil
}

// waitRestarted waits until each of targets, a Deployment restartTargets
// stamped, has the Ready pods want says. A target it did not find is
// not waited for.
func (i *Installer) waitRestarted(ctx context.Context, targets []restartTarget, want map[restartTarget]int,
	stamp string, deadline time.Time, timeout time.Duration) error {
	for _, t := range targets {
		n, ok := want[t]
		if !ok {
			continue
		}
		for {
			ready, err := i.restartedReady(ctx, t, stamp)
			if err == nil && ready >= n {
				break
			}
			if time.Now().After(deadline) {
				if err == nil {
					err = fmt.Errorf("%d of %d new pods Ready", ready, n)
				}
				return fmt.Errorf("%w: %s/%s did not become ready within %s: %w", ErrInstallFailed, t.namespace, t.name, timeout, err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(knativeRestartEvery):
			}
		}
	}
	return nil
}

// restartedReady counts the pods of Deployment t from the template
// restartTargets stamped that are Ready and not being deleted. A
// Deployment's pods are named after it.
func (i *Installer) restartedReady(ctx context.Context, t restartTarget, stamp string) (int, error) {
	out, err := i.kubectl(ctx, "", "-n", t.namespace, "get", "pods", "-o",
		`jsonpath={range .items[*]}{.metadata.name}|{.metadata.annotations.cloudburrow\.dev/restarted-at}|`+
			`{.status.conditions[?(@.type=="Ready")].status}|{.metadata.deletionTimestamp}{"\n"}{end}`)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		parts := append(strings.Split(strings.TrimSpace(line), "|"), "")
		rest, ok := strings.CutPrefix(parts[0], t.name+"-")
		// <deployment>-<template hash>-<suffix>: not another
		// Deployment's whose name extends this one's.
		if ok && strings.Count(rest, "-") == 1 && len(parts) == 5 &&
			parts[1] == stamp && parts[2] == "True" && parts[3] == "" {
			n++
		}
	}
	return n, nil
}

// RevisionProgressDeadline is how long a new Cloud Run revision has to become
// ready before it is reported failed.
//
// Knative's default is 600s, and a container that exits at startup is not
// declared failed until then: the compat suite measured a startup failure
// reported after 10m2s, with the container's own output already in hand
// (#568). Cloud Run gives a container 240s by default (its startup probe:
// timeoutSeconds 240, periodSeconds 240, failureThreshold 1), and this is
// that number, so a broken deployment is reported here no later than there.
const RevisionProgressDeadline = "240s"

// ConfigureDeployment sets the revision progress deadline in Knative's
// config-deployment, and the features in config-features (KnativeFeatures).
//
// It runs on a fresh install and on every `up` against a cluster that
// already has Knative, so a cluster created before the setting existed gets
// it too; a merge patch of one key is idempotent. Knative validates the
// ConfigMap through a webhook, so the caller waits for Knative first.
func (i *Installer) ConfigureDeployment(ctx context.Context) error {
	patch := fmt.Sprintf(`{"data":{"progress-deadline":%q}}`, RevisionProgressDeadline)
	if err := i.patchValidated(ctx, "patch", "configmap/config-deployment",
		"-n", "knative-serving", "--type", "merge", "-p", patch); err != nil {
		return fmt.Errorf("%w: set the revision progress deadline: %w", ErrInstallFailed, err)
	}
	if err := i.patchValidated(ctx, "patch", "configmap/config-features",
		"-n", "knative-serving", "--type", "merge", "-p", KnativeFeatures); err != nil {
		return fmt.Errorf("%w: enable the Knative features Cloud Run's volumes need: %w", ErrInstallFailed, err)
	}
	return nil
}

// KnativeFeatures are the config-features flags Cloud Run's volumes need.
// NFS (#1448): Knative refuses an inline nfs volume, so a service's NFS
// volume is a PersistentVolume of the NFS export and a claim bound to it,
// which a revision may mount, and write when the volume is not read-only.
// Cloud Storage FUSE (#1474): the bucket is mounted on the node by a gcsfuse
// pod, and a revision mounts that host directory with HostToContainer
// propagation, so it sees the FUSE mount made under it.
const KnativeFeatures = `{"data":{"kubernetes.podspec-persistent-volume-claim":"enabled",` +
	`"kubernetes.podspec-persistent-volume-write":"enabled",` +
	`"kubernetes.podspec-volumes-hostpath":"enabled",` +
	`"kubernetes.podspec-volumes-mount-propagation":"enabled"}}`

// webhookRetry bounds how long a patch of a ConfigMap Knative validates is
// retried while its webhook refuses calls, and webhookRetryEvery spaces the
// attempts. The Deployments being Available is not enough: after stop and
// up, the webhook still answered "failed calling webhook" to the
// progress-deadline patch that followed WaitKnative, in the run shard's
// second up (#765).
var (
	webhookRetry      = 2 * time.Minute
	webhookRetryEvery = 500 * time.Millisecond
)

// patchValidated runs a kubectl patch of a ConfigMap in knative-serving,
// which Knative's webhook validates, and retries it while the webhook is not
// yet answering. Any other error is returned at once.
func (i *Installer) patchValidated(ctx context.Context, args ...string) error {
	deadline := time.Now().Add(webhookRetry)
	for {
		_, err := i.kubectl(ctx, "", args...)
		if err == nil || !strings.Contains(err.Error(), "failed calling webhook") || time.Now().After(deadline) {
			return err
		}
		i.logf("  knative's webhook is not answering yet; retrying %s\n", args[1])
		select {
		case <-ctx.Done():
			return err
		case <-time.After(webhookRetryEvery):
		}
	}
}

// KnativeInstalled reports whether Knative Serving is already present, so
// installation is idempotent.
func (i *Installer) KnativeInstalled(ctx context.Context) bool {
	_, err := i.kubectl(ctx, "", "get", "namespace", "knative-serving")
	return err == nil
}

func shortURL(u string) string {
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[i+1:]
	}
	return u
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Installer exposes its kubectl for callers that need a raw operation.
func (i *Installer) Kubectl(ctx context.Context, args ...string) (string, error) {
	return i.kubectl(ctx, "", args...)
}

// KubectlIn runs kubectl with stdin, which may be binary: a file copied
// into a pod with exec (#1384).
func (i *Installer) KubectlIn(ctx context.Context, stdin string, args ...string) (string, error) {
	return i.kubectl(ctx, stdin, args...)
}
