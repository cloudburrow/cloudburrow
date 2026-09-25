package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Kubernetes object conventions.
const (
	// OwnerLabel marks every object CloudBurrow created, so cleanup can never
	// touch something it did not.
	OwnerLabel = "cloudburrow.dev/owned"
	// ProjectLabel scopes an object to a project, so a listing can be filtered
	// server side rather than by reading every secret in the namespace.
	ProjectLabel = "cloudburrow.dev/project"
	// ServiceLabel identifies these objects as Secret Manager's, so they are
	// not confused with a Secret a developer created by hand.
	ServiceLabel = "cloudburrow.dev/service"
	// ServiceLabelValue is the value of ServiceLabel.
	ServiceLabelValue = "secretmanager"

	// SecretAnnotation carries the Secret Manager metadata for the secret.
	SecretAnnotation = "cloudburrow.dev/secret"
	// VersionAnnotationPrefix carries per-version metadata; the version number
	// is appended.
	VersionAnnotationPrefix = "cloudburrow.dev/version-"
	// SecretIDAnnotation records the original, unsanitised secret ID, because
	// the Kubernetes object name is lossy.
	SecretIDAnnotation = "cloudburrow.dev/secret-id"
)

// Runner executes kubectl. Injected so every code path is testable without a
// cluster.
type Runner interface {
	Run(ctx context.Context, stdin string, args ...string) (string, error)
}

// KubectlRunner is the real runner.
type KubectlRunner struct {
	Kubeconfig string
}

func (k KubectlRunner) Run(ctx context.Context, stdin string, args ...string) (string, error) {
	full := append([]string{"--kubeconfig", k.Kubeconfig}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", full...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errOut.String()); msg != "" {
			return out.String(), fmt.Errorf("kubectl: %w: %s", err, msg)
		}
		return out.String(), fmt.Errorf("kubectl: %w", err)
	}
	return out.String(), nil
}

// compile-time proof that a KubeStore is usable wherever the CLI store is,
// so swapping backends cannot silently fall back to the wrong one.
var _ store.Store = (*KubeStore)(nil)

// KubeStore persists Secret Manager state as Kubernetes Secrets.
//
// It implements store.Store, but it is **not** a general-purpose key/value
// store: it understands this package's key shapes deliberately. A generic
// blob store could hold the same bytes, but it could not produce a Kubernetes
// Secret whose `data` keys a pod can reference through `secretKeyRef` — and
// that is the entire point of backing secrets with the cluster.
//
// One GCP secret maps to one Kubernetes Secret:
//
//	data["v1"], data["v2"], ...   the raw payloads, which a pod can mount
//	annotations[...secret]        the Secret Manager metadata
//	annotations[...version-1]     per-version metadata (state, timestamps)
//
// The payload lives in `data` rather than inside the metadata JSON precisely
// so that a pod can consume it without CloudBurrow being in the path.
type KubeStore struct {
	runner    Runner
	namespace string
	instance  string

	// mu serialises read-modify-write cycles. Every write is a patch against
	// an object that may hold other versions, so two concurrent writes
	// without this would lose one.
	mu sync.Mutex

	// epoch, when set, labels every Secret this process writes, so an
	// ephemeral run can delete what an earlier run left without touching what
	// it wrote itself (#483).
	epoch string
}

// NewKubeStore returns a Kubernetes-backed store.
func NewKubeStore(r Runner, namespace, instance string) *KubeStore {
	return &KubeStore{runner: r, namespace: namespace, instance: instance}
}

// EpochLabel marks a Secret written by an ephemeral run.
const EpochLabel = "cloudburrow.dev/epoch"

// SetEpoch labels this process's writes with epoch; see Forget.
func (k *KubeStore) SetEpoch(epoch string) { k.epoch = epoch }

// Forget deletes the Secrets --mode says must not survive a restart (#483).
// With an epoch (ephemeral mode) that is every Secret Manager Secret of this
// instance not written by this process; `!=` also matches Secrets without
// the label, which a persistent run wrote. Without one (persistent mode) it
// is every Secret an ephemeral run wrote. It needs the instance, so it can
// never select another instance's Secrets.
func (k *KubeStore) Forget(ctx context.Context) error {
	if k.instance == "" {
		return errors.New("forget Secret Manager state: no instance to select by")
	}
	sel := ServiceLabel + "=" + ServiceLabelValue + ",cloudburrow.dev/instance=" + labelSafe(k.instance) + "," + EpochLabel
	if k.epoch != "" {
		sel += "!=" + k.epoch
	}
	if _, err := k.runner.Run(ctx, "", "-n", k.namespace, "delete", "secrets", "-l", sel); err != nil {
		return fmt.Errorf("delete Secret Manager Secrets from an earlier run: %w", err)
	}
	return nil
}

// kubeSecret is the subset of a v1.Secret this store reads.
type kubeSecret struct {
	Metadata struct {
		Name        string            `json:"name"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
}

// parseKey splits one of this package's storage keys.
func parseKey(key string) (kind, project, id string, number int, err error) {
	parts := strings.Split(key, "/")
	switch {
	case len(parts) == 3 && parts[0] == "secret":
		return "secret", parts[1], parts[2], 0, nil
	case len(parts) == 4 && parts[0] == "version":
		n, convErr := strconv.Atoi(parts[3])
		if convErr != nil {
			return "", "", "", 0, fmt.Errorf("version key %q has a non-numeric version", key)
		}
		return "version", parts[1], parts[2], n, nil
	default:
		return "", "", "", 0, fmt.Errorf("key %q is not a Secret Manager key", key)
	}
}

// fetch reads the Kubernetes Secret backing a GCP secret. A missing object is
// reported with found=false rather than as an error, because "not there yet"
// is the normal case on a first write.
func (k *KubeStore) fetch(ctx context.Context, project, id string) (kubeSecret, bool, error) {
	name := KubernetesSecretName(project, id)
	out, err := k.runner.Run(ctx, "", "-n", k.namespace, "get", "secret", name, "-o", "json")
	if err != nil {
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "not found") {
			return kubeSecret{}, false, nil
		}
		return kubeSecret{}, false, err
	}
	var ks kubeSecret
	if err := json.Unmarshal([]byte(out), &ks); err != nil {
		return kubeSecret{}, false, fmt.Errorf("decode secret %s: %w", name, err)
	}
	return ks, true, nil
}

// apply writes the object back.
//
// It applies a complete object rather than patching, so a version removed
// from the map is actually removed. A strategic patch would merge it back.
func (k *KubeStore) apply(ctx context.Context, project, id string, ks kubeSecret) error {
	ks.Metadata.Name = KubernetesSecretName(project, id)
	if ks.Metadata.Labels == nil {
		ks.Metadata.Labels = map[string]string{}
	}
	ks.Metadata.Labels[OwnerLabel] = "true"
	ks.Metadata.Labels[ServiceLabel] = ServiceLabelValue
	ks.Metadata.Labels[ProjectLabel] = labelSafe(project)
	if k.instance != "" {
		ks.Metadata.Labels["cloudburrow.dev/instance"] = labelSafe(k.instance)
	}
	if k.epoch != "" {
		ks.Metadata.Labels[EpochLabel] = k.epoch
	} else {
		// A persistent write takes over a Secret an ephemeral run labelled.
		delete(ks.Metadata.Labels, EpochLabel)
	}
	if ks.Metadata.Annotations == nil {
		ks.Metadata.Annotations = map[string]string{}
	}
	ks.Metadata.Annotations[SecretIDAnnotation] = id
	ks.Metadata.Annotations["cloudburrow.dev/secret-project"] = project

	manifest, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "Opaque",
		"metadata": map[string]any{
			"name":        ks.Metadata.Name,
			"namespace":   k.namespace,
			"labels":      ks.Metadata.Labels,
			"annotations": ks.Metadata.Annotations,
		},
		"data": ks.Data,
	})
	if err != nil {
		return fmt.Errorf("encode secret manifest: %w", err)
	}
	// Server-side apply, for two reasons that both matter here.
	//
	// A client-side apply records the whole object, `data` included, in the
	// kubectl.kubernetes.io/last-applied-configuration annotation — which
	// would put every payload back in an annotation, exactly where splitting
	// it out of the metadata was meant to keep it from being.
	//
	// And --force-conflicts takes ownership of fields another manager holds
	// rather than failing halfway, which would leave the API reporting a
	// write the cluster did not take.
	_, err = k.runner.Run(ctx, string(manifest), "-n", k.namespace,
		"apply", "--server-side", "--force-conflicts",
		"--field-manager", "cloudburrow-secretmanager", "-f", "-")
	return err
}

// labelSafe renders a value that Kubernetes accepts as a label.
//
// Label values are far more restricted than project IDs, so an unsafe one is
// replaced rather than rejected: the label is for filtering, and the
// authoritative project is in an annotation.
func labelSafe(v string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(v) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-._")
	if out == "" {
		out = "unnamed"
	}
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}

// Get returns the stored value for a key.
func (k *KubeStore) Get(key string) ([]byte, error) {
	kind, project, id, number, err := parseKey(key)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()

	k.mu.Lock()
	defer k.mu.Unlock()

	ks, found, err := k.fetch(ctx, project, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("key %q not found", key)
	}

	if kind == "secret" {
		raw, ok := ks.Metadata.Annotations[SecretAnnotation]
		if !ok {
			return nil, fmt.Errorf("key %q not found", key)
		}
		return []byte(raw), nil
	}

	raw, ok := ks.Metadata.Annotations[VersionAnnotationPrefix+strconv.Itoa(number)]
	if !ok {
		return nil, fmt.Errorf("key %q not found", key)
	}
	var v Version
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("decode version metadata: %w", err)
	}
	// The payload lives in data, not in the metadata, so it is re-attached on
	// the way out. A destroyed version has no data key and keeps none.
	if encoded, ok := ks.Data[VersionKey(number)]; ok {
		payload, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode payload for %s: %w", key, decodeErr)
		}
		v.Payload = payload
	}
	return json.Marshal(v)
}

// Put writes a value.
func (k *KubeStore) Put(key string, value []byte) error {
	kind, project, id, number, err := parseKey(key)
	if err != nil {
		return err
	}
	ctx := context.Background()

	k.mu.Lock()
	defer k.mu.Unlock()

	ks, _, err := k.fetch(ctx, project, id)
	if err != nil {
		return err
	}
	if ks.Metadata.Annotations == nil {
		ks.Metadata.Annotations = map[string]string{}
	}
	if ks.Data == nil {
		ks.Data = map[string]string{}
	}

	if kind == "secret" {
		ks.Metadata.Annotations[SecretAnnotation] = string(value)
		return k.apply(ctx, project, id, ks)
	}

	var v Version
	if err := json.Unmarshal(value, &v); err != nil {
		return fmt.Errorf("decode version for %s: %w", key, err)
	}
	payload := v.Payload
	// The payload is split out so a pod can reference it directly. Leaving a
	// copy in the annotation would put the bytes somewhere `kubectl describe`
	// prints them.
	v.Payload = nil

	meta, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode version metadata: %w", err)
	}
	ks.Metadata.Annotations[VersionAnnotationPrefix+strconv.Itoa(number)] = string(meta)

	if len(payload) > 0 {
		ks.Data[VersionKey(number)] = base64.StdEncoding.EncodeToString(payload)
	} else {
		// A destroyed version has no payload, and its key must actually go
		// away rather than hold an empty string a caller could read back.
		delete(ks.Data, VersionKey(number))
	}
	return k.apply(ctx, project, id, ks)
}

// Delete removes a key.
func (k *KubeStore) Delete(key string) error {
	kind, project, id, number, err := parseKey(key)
	if err != nil {
		return err
	}
	ctx := context.Background()

	k.mu.Lock()
	defer k.mu.Unlock()

	ks, found, err := k.fetch(ctx, project, id)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}

	if kind == "secret" {
		// Deleting the secret removes the whole object, versions included.
		name := KubernetesSecretName(project, id)
		_, err := k.runner.Run(ctx, "", "-n", k.namespace, "delete", "secret", name, "--ignore-not-found")
		return err
	}

	delete(ks.Metadata.Annotations, VersionAnnotationPrefix+strconv.Itoa(number))
	delete(ks.Data, VersionKey(number))
	return k.apply(ctx, project, id, ks)
}

// List returns keys under a prefix, sorted.
func (k *KubeStore) List(prefix string) ([]string, error) {
	ctx := context.Background()

	k.mu.Lock()
	defer k.mu.Unlock()

	selector := fmt.Sprintf("%s=true,%s=%s", OwnerLabel, ServiceLabel, ServiceLabelValue)
	out, err := k.runner.Run(ctx, "", "-n", k.namespace, "get", "secret",
		"-l", selector, "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []kubeSecret `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("decode secret list: %w", err)
	}

	var keys []string
	for _, ks := range list.Items {
		project := ks.Metadata.Annotations["cloudburrow.dev/secret-project"]
		id := ks.Metadata.Annotations[SecretIDAnnotation]
		if project == "" || id == "" {
			// An object without our annotations is not ours to interpret,
			// whatever its labels say.
			continue
		}
		if _, ok := ks.Metadata.Annotations[SecretAnnotation]; ok {
			keys = append(keys, secretKey(project, id))
		}
		for ann := range ks.Metadata.Annotations {
			rest, ok := strings.CutPrefix(ann, VersionAnnotationPrefix)
			if !ok {
				continue
			}
			n, convErr := strconv.Atoi(rest)
			if convErr != nil {
				continue
			}
			keys = append(keys, versionKey(project, id, n))
		}
	}

	filtered := keys[:0]
	for _, key := range keys {
		if strings.HasPrefix(key, prefix) {
			filtered = append(filtered, key)
		}
	}
	sort.Strings(filtered)
	return filtered, nil
}

// Commit applies a set of writes.
//
// They are applied one at a time. The Kubernetes API offers no cross-object
// transaction, so claiming atomicity here would be a promise this store
// cannot keep — and a caller that believed it would skip its own recovery.
func (k *KubeStore) Commit(ops []store.Op) error {
	for _, op := range ops {
		var err error
		switch op.Kind {
		case store.OpDelete:
			err = k.Delete(op.Key)
		default:
			err = k.Put(op.Key, op.Value)
		}
		if err != nil {
			return fmt.Errorf("commit %s: %w", op.Key, err)
		}
	}
	return nil
}

// Close releases nothing: the cluster owns the state.
func (k *KubeStore) Close() error { return nil }
