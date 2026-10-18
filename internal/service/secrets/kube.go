package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Kubernetes object conventions.
const (
	// OwnerLabel marks every object CloudBurrow created, so cleanup can never
	// touch something it did not.
	OwnerLabel = k8s.OwnedLabel
	// ProjectLabel scopes an object to a project, so a listing can be filtered
	// server side rather than by reading every secret in the namespace.
	ProjectLabel = "cloudburrow.dev/project"
	// ServiceLabel identifies these objects as Secret Manager's, so they are
	// not confused with a Secret a developer created by hand.
	ServiceLabel = k8s.ServiceLabel
	// ServiceLabelValue is the value of ServiceLabel.
	ServiceLabelValue = "secretmanager"

	// SecretAnnotation carries the Secret Manager metadata for the secret.
	SecretAnnotation = "cloudburrow.dev/secret"
	// VersionAnnotationPrefix carried per-version metadata on the secret's
	// Secret before #1596, the version number appended; load migrates it.
	VersionAnnotationPrefix = "cloudburrow.dev/version-"
	// SecretIDAnnotation records the original, unsanitised secret ID, because
	// the Kubernetes object name is lossy.
	SecretIDAnnotation = "cloudburrow.dev/secret-id"
)

// Version Secret conventions (#1596).
const (
	// VersionMetadataAnnotation carries a version's metadata on the Secret
	// that stores it.
	VersionMetadataAnnotation = "cloudburrow.dev/secret-version"
	// versionNumberAnnotation is that version's number.
	versionNumberAnnotation = "cloudburrow.dev/secret-version-number"
	// SecretKeyLabel is on a secret's Secret and on each of its version
	// Secrets: a digest of the project and ID, so they can be deleted with
	// one DeleteCollection.
	SecretKeyLabel = "cloudburrow.dev/secret-key"
	// projectAnnotation is the original project.
	projectAnnotation = "cloudburrow.dev/secret-project"
)

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
// One GCP secret is one Kubernetes Secret, KubernetesSecretName, holding its
// metadata in an annotation, and each of its versions is a Secret of its
// own, VersionSecretName (#1596):
//
//	data["v1"]                       the raw payload, which a pod can mount
//	annotations[...secret-version]   the version's metadata (state, times)
//
// Kubernetes caps an object's annotations at 256 KiB and a Secret at 1 MiB,
// and Secret Manager documents no cap on a secret's versions
// (docs.cloud.google.com/secret-manager/quotas: a payload is at most
// 64 KiB), so a version per Secret keeps every object small whatever the
// count. Until #1596 every version's metadata was an annotation on the one
// Secret, and the 905th version failed; load migrates that layout.
//
// The payload lives in `data` rather than inside the metadata JSON precisely
// so that a pod can consume it without CloudBurrow being in the path.
//
// Reads are answered from memory: the front is the only writer of these
// Secrets, so they are listed once, when the store is first used, and every
// write updates the copy after the cluster has taken it. A call costs no
// kubectl for a read, and one small apply for a write, however many versions
// a secret has.
type KubeStore struct {
	kube      *k8s.Runner
	namespace string
	instance  string

	// mu serialises every call, so the cache and the cluster agree.
	mu sync.Mutex

	// epoch, when set, labels every Secret this process writes, so an
	// ephemeral run can delete what an earlier run left without touching what
	// it wrote itself (#483).
	epoch string

	// loaded says secrets holds what the cluster holds; it is cleared by a
	// bulk delete, so the next call lists again.
	loaded  bool
	secrets map[string]*cachedSecret
}

// cachedSecret is what the cluster holds for one GCP secret.
type cachedSecret struct {
	project, id string
	// meta is the secret's metadata; nil until it is written.
	meta []byte
	// head is whether its Secret exists.
	head bool
	// legacy is the payloads the Secret itself held before #1596, kept so
	// a revision deployed then, which references them, still starts.
	legacy map[string]string
	// versions are the values Get returns, by number.
	versions map[int][]byte
}

// NewKubeStore returns a store over Secrets in the runner's namespace.
func NewKubeStore(kube *k8s.Runner, instance string) *KubeStore {
	return &KubeStore{kube: kube, namespace: kube.Namespace(), instance: instance}
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
	sel := ServiceLabel + "=" + ServiceLabelValue + "," + k8s.InstanceLabel + "=" + labelSafe(k.instance) + "," + EpochLabel
	if k.epoch != "" {
		sel += "!=" + k.epoch
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.loaded = false
	if err := k.deleteSelected(ctx, sel); err != nil {
		return fmt.Errorf("delete Secret Manager Secrets from an earlier run: %w", err)
	}
	return nil
}

// Load lists the cluster's Secrets and migrates any in the layout before
// #1596, which a call does anyway the first time; the instance calls it when
// it starts, so the first request does not wait for it.
func (k *KubeStore) Load() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.load(context.Background())
}

// DeleteAll removes every Secret Manager Secret List reads, for
// /admin/reset (Store.Reset), with one DeleteCollection.
func (k *KubeStore) DeleteAll() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.loaded = false
	sel := k8s.OwnedSelector + "," + ServiceLabel + "=" + ServiceLabelValue
	if err := k.deleteSelected(context.Background(), sel); err != nil {
		return fmt.Errorf("delete Secret Manager Secrets: %w", err)
	}
	return nil
}

// DeleteSecretRecords removes a secret and every version of it, with one
// DeleteCollection, rather than a delete per version (Store.deleteSecret).
func (k *KubeStore) DeleteSecretRecords(project, id string) error {
	ctx := context.Background()
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.load(ctx); err != nil {
		return err
	}
	sel := k8s.OwnedSelector + "," + ServiceLabel + "=" + ServiceLabelValue + "," + SecretKeyLabel + "=" + secretKeyLabel(project, id)
	if err := k.deleteSelected(ctx, sel); err != nil {
		return fmt.Errorf("delete the Secrets of %s: %w", SecretName(project, id), err)
	}
	if err := k.kube.Delete(ctx, "secret", KubernetesSecretName(project, id), true); err != nil {
		return err
	}
	delete(k.secrets, cacheKey(project, id))
	return nil
}

// deleteSelected deletes the Secrets matching a label selector with one
// DeleteCollection request, which the API server carries out itself.
// `kubectl delete secrets -l` lists them and sends one DELETE per Secret, so
// its time grows with what earlier runs left; the same pattern made KMS's
// /admin/reset outlast its bound with a few hundred Secrets (#1219, #1232).
func (k *KubeStore) deleteSelected(ctx context.Context, selector string) error {
	path := "/api/v1/namespaces/" + url.PathEscape(k.namespace) + "/secrets?labelSelector=" + url.QueryEscape(selector)
	_, err := k.kube.Do(ctx, "", "delete", "--raw", path)
	return err
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

// VersionSecretName is the Kubernetes Secret that stores one version of a
// secret, and the one a workload referencing it mounts (#1596). A secret's
// own name always ends in a hex digest, which "v" is not, so the two never
// collide.
func VersionSecretName(project, secret string, number int) string {
	return KubernetesSecretName(project, secret) + "-v" + strconv.Itoa(number)
}

// secretKeyLabel is SecretKeyLabel's value for a secret.
func secretKeyLabel(project, id string) string {
	sum := sha256.Sum256([]byte(project + "/" + id))
	return hex.EncodeToString(sum[:20])
}

func cacheKey(project, id string) string { return project + "/" + id }

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

// load lists every Secret Manager Secret into the cache, once, and migrates
// any secret still in the layout before #1596. The caller holds k.mu.
func (k *KubeStore) load(ctx context.Context) error {
	if k.loaded {
		return nil
	}
	selector := fmt.Sprintf("%s,%s=%s", k8s.OwnedSelector, ServiceLabel, ServiceLabelValue)
	out, err := k.kube.List(ctx, "secret", selector, "json")
	if err != nil {
		return err
	}
	var list struct {
		Items []kubeSecret `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return fmt.Errorf("decode secret list: %w", err)
	}
	cache, old, err := readSecrets(list.Items)
	if err != nil {
		return err
	}
	k.secrets = cache
	for _, o := range old {
		if err := k.migrate(ctx, o); err != nil {
			return fmt.Errorf("migrate %s to a Secret per version: %w", SecretName(o.project, o.id), err)
		}
	}
	k.loaded = true
	return nil
}

// oldLayout is a secret whose versions are annotations on its own Secret,
// as before #1596: their values, by number.
type oldLayout struct {
	project, id string
	versions    map[int][]byte
}

// readSecrets builds the cache from listed Secrets, and returns the secrets
// in the old layout, whose versions it holds as though migrated.
func readSecrets(items []kubeSecret) (map[string]*cachedSecret, []oldLayout, error) {
	cache := map[string]*cachedSecret{}
	entry := func(project, id string) *cachedSecret {
		c := cache[cacheKey(project, id)]
		if c == nil {
			c = &cachedSecret{project: project, id: id, versions: map[int][]byte{}}
			cache[cacheKey(project, id)] = c
		}
		return c
	}
	var old []oldLayout
	for _, ks := range items {
		project := ks.Metadata.Annotations[projectAnnotation]
		id := ks.Metadata.Annotations[SecretIDAnnotation]
		if project == "" || id == "" {
			// An object without our annotations is not ours to interpret,
			// whatever its labels say.
			continue
		}
		if meta, ok := ks.Metadata.Annotations[VersionMetadataAnnotation]; ok {
			n, err := strconv.Atoi(ks.Metadata.Annotations[versionNumberAnnotation])
			if err != nil || ks.Metadata.Name != VersionSecretName(project, id, n) {
				continue
			}
			v, err := versionValue(meta, ks.Data[VersionKey(n)])
			if err != nil {
				continue
			}
			entry(project, id).versions[n] = v
			continue
		}
		if ks.Metadata.Name != KubernetesSecretName(project, id) {
			continue
		}
		c := entry(project, id)
		c.head = true
		if meta, ok := ks.Metadata.Annotations[SecretAnnotation]; ok {
			c.meta = []byte(meta)
		}
		if len(ks.Data) > 0 {
			c.legacy = ks.Data
		}
		o := oldLayout{project: project, id: id, versions: map[int][]byte{}}
		for ann, meta := range ks.Metadata.Annotations {
			rest, ok := strings.CutPrefix(ann, VersionAnnotationPrefix)
			if !ok {
				continue
			}
			n, err := strconv.Atoi(rest)
			if err != nil {
				continue
			}
			v, err := versionValue(meta, ks.Data[VersionKey(n)])
			if err != nil {
				return nil, nil, fmt.Errorf("decode version %d of %s: %w", n, SecretName(project, id), err)
			}
			o.versions[n] = v
		}
		if len(o.versions) > 0 {
			old = append(old, o)
		}
	}
	// A version already in a Secret of its own (a migration cut short)
	// wins over its annotation; the rest are cached as they will be.
	for _, o := range old {
		c := cache[cacheKey(o.project, o.id)]
		for n, v := range o.versions {
			if _, ok := c.versions[n]; !ok {
				c.versions[n] = v
			} else {
				delete(o.versions, n)
			}
		}
	}
	return cache, old, nil
}

// migrate writes each version of a secret in the old layout to a Secret of
// its own, and then rewrites the secret's Secret without the version
// annotations. It keeps the payloads there, as a revision deployed before
// the migration references them. Each step is idempotent, so a migration
// cut short is finished by the next load. The caller holds k.mu.
func (k *KubeStore) migrate(ctx context.Context, o oldLayout) error {
	numbers := make([]int, 0, len(o.versions))
	for n := range o.versions {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	for _, n := range numbers {
		if err := k.applyVersion(ctx, o.project, o.id, n, o.versions[n]); err != nil {
			return err
		}
	}
	return k.applyHead(ctx, k.secrets[cacheKey(o.project, o.id)])
}

// versionValue is the value Get returns for a version: its metadata with the
// payload, from data, re-attached. A destroyed version has no data key.
func versionValue(meta, encoded string) ([]byte, error) {
	var v Version
	if err := json.Unmarshal([]byte(meta), &v); err != nil {
		return nil, fmt.Errorf("decode version metadata: %w", err)
	}
	if encoded != "" {
		payload, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode payload: %w", err)
		}
		v.Payload = payload
	}
	return json.Marshal(v)
}

// labels are the labels of every Secret of a secret.
func (k *KubeStore) labels(project, id string) map[string]string {
	labels := map[string]string{
		OwnerLabel:     k8s.OwnedValue,
		ServiceLabel:   ServiceLabelValue,
		ProjectLabel:   labelSafe(project),
		SecretKeyLabel: secretKeyLabel(project, id),
	}
	if k.instance != "" {
		labels[k8s.InstanceLabel] = labelSafe(k.instance)
	}
	if k.epoch != "" {
		// A persistent write leaves it off, so it takes over a Secret an
		// ephemeral run labelled.
		labels[EpochLabel] = k.epoch
	}
	return labels
}

// applyHead writes a secret's own Secret: its metadata, and the payloads
// kept from before #1596.
func (k *KubeStore) applyHead(ctx context.Context, c *cachedSecret) error {
	annotations := map[string]string{
		SecretIDAnnotation: c.id,
		projectAnnotation:  c.project,
	}
	if c.meta != nil {
		annotations[SecretAnnotation] = string(c.meta)
	}
	if err := k.apply(ctx, KubernetesSecretName(c.project, c.id), k.labels(c.project, c.id), annotations, c.legacy); err != nil {
		return err
	}
	c.head = true
	return nil
}

// applyVersion writes the Secret of one version, value being what Get
// returns for it.
func (k *KubeStore) applyVersion(ctx context.Context, project, id string, number int, value []byte) error {
	var v Version
	if err := json.Unmarshal(value, &v); err != nil {
		return fmt.Errorf("decode version %d: %w", number, err)
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
	data := map[string]string{}
	if len(payload) > 0 {
		data[VersionKey(number)] = base64.StdEncoding.EncodeToString(payload)
	}
	// A destroyed version has no payload, and its key goes away rather than
	// hold an empty string a caller could read back.
	return k.apply(ctx, VersionSecretName(project, id, number), k.labels(project, id), map[string]string{
		SecretIDAnnotation:        id,
		projectAnnotation:         project,
		VersionMetadataAnnotation: string(meta),
		versionNumberAnnotation:   strconv.Itoa(number),
	}, data)
}

// apply writes a whole Secret.
//
// It applies a complete object rather than patching, so a key removed from
// a map is actually removed. A strategic patch would merge it back.
func (k *KubeStore) apply(ctx context.Context, name string, labels, annotations, data map[string]string) error {
	if data == nil {
		data = map[string]string{}
	}
	manifest, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "Opaque",
		"metadata": map[string]any{
			"name":        name,
			"namespace":   k.namespace,
			"labels":      labels,
			"annotations": annotations,
		},
		"data": data,
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
	if err := k.kube.Apply(ctx, string(manifest), k8s.ApplyOptions{
		ServerSide: true, ForceConflicts: true, FieldManager: "cloudburrow-secretmanager",
	}); err != nil {
		return fmt.Errorf("apply Secret %s: %w", name, err)
	}
	return nil
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
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.load(context.Background()); err != nil {
		return nil, err
	}
	c := k.secrets[cacheKey(project, id)]
	if c != nil {
		if kind == "secret" && c.meta != nil {
			return append([]byte(nil), c.meta...), nil
		}
		if v, ok := c.versions[number]; kind == "version" && ok {
			return append([]byte(nil), v...), nil
		}
	}
	return nil, fmt.Errorf("key %q not found", key)
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
	if err := k.load(ctx); err != nil {
		return err
	}
	c := k.secrets[cacheKey(project, id)]
	if c == nil {
		c = &cachedSecret{project: project, id: id, versions: map[int][]byte{}}
	}

	if kind == "secret" {
		was := c.meta
		c.meta = append([]byte(nil), value...)
		if err := k.applyHead(ctx, c); err != nil {
			c.meta = was
			return err
		}
		k.secrets[cacheKey(project, id)] = c
		return nil
	}

	// What Get returns: the value as decoded and encoded again, so the
	// cache holds exactly what a reload would.
	var v Version
	if err := json.Unmarshal(value, &v); err != nil {
		return fmt.Errorf("decode version for %s: %w", key, err)
	}
	canonical, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode version for %s: %w", key, err)
	}
	if err := k.applyVersion(ctx, project, id, number, canonical); err != nil {
		return err
	}
	c.versions[number] = canonical
	k.secrets[cacheKey(project, id)] = c
	if len(v.Payload) == 0 {
		// A destroyed version's payload goes from the secret's own Secret
		// too, where it was kept from before #1596.
		return k.dropLegacy(ctx, c, number)
	}
	return nil
}

// dropLegacy removes a version's payload kept on the secret's own Secret.
func (k *KubeStore) dropLegacy(ctx context.Context, c *cachedSecret, number int) error {
	if _, ok := c.legacy[VersionKey(number)]; !ok || !c.head {
		return nil
	}
	delete(c.legacy, VersionKey(number))
	return k.applyHead(ctx, c)
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
	if err := k.load(ctx); err != nil {
		return err
	}
	c := k.secrets[cacheKey(project, id)]
	if c == nil {
		return nil
	}

	if kind == "secret" {
		// Deleting the secret removes its own Secret; Store.deleteSecret
		// deletes the versions first, or DeleteSecretRecords all at once.
		if err := k.kube.Delete(ctx, "secret", KubernetesSecretName(project, id), true); err != nil {
			return err
		}
		c.meta, c.head, c.legacy = nil, false, nil
		if len(c.versions) == 0 {
			delete(k.secrets, cacheKey(project, id))
		}
		return nil
	}

	if _, ok := c.versions[number]; ok {
		if err := k.kube.Delete(ctx, "secret", VersionSecretName(project, id, number), true); err != nil {
			return err
		}
		delete(c.versions, number)
	}
	if err := k.dropLegacy(ctx, c, number); err != nil {
		return err
	}
	if !c.head && len(c.versions) == 0 {
		delete(k.secrets, cacheKey(project, id))
	}
	return nil
}

// List returns keys under a prefix, sorted.
func (k *KubeStore) List(prefix string) ([]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.load(context.Background()); err != nil {
		return nil, err
	}
	var keys []string
	k.each(prefix, func(key string, _ []byte) { keys = append(keys, key) })
	sort.Strings(keys)
	return keys, nil
}

// Scan returns every value under prefix, from memory, as List does.
func (k *KubeStore) Scan(prefix string) (map[string][]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.load(context.Background()); err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	k.each(prefix, func(key string, v []byte) { out[key] = append([]byte(nil), v...) })
	return out, nil
}

// each calls fn for every key under prefix and its value. A prefix naming
// one secret's versions, as the Store's listings of them do, reads only that
// secret. The caller holds k.mu.
func (k *KubeStore) each(prefix string, fn func(key string, value []byte)) {
	visit := func(c *cachedSecret) {
		if c.meta != nil {
			if key := secretKey(c.project, c.id); strings.HasPrefix(key, prefix) {
				fn(key, c.meta)
			}
		}
		for n, v := range c.versions {
			if key := versionKey(c.project, c.id, n); strings.HasPrefix(key, prefix) {
				fn(key, v)
			}
		}
	}
	if parts := strings.Split(prefix, "/"); len(parts) == 4 && parts[0] == "version" && parts[3] == "" {
		if c := k.secrets[cacheKey(parts[1], parts[2])]; c != nil {
			visit(c)
		}
		return
	}
	for _, c := range k.secrets {
		visit(c)
	}
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
