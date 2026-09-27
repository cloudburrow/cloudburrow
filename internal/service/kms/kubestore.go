package kms

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// KubeStore keeps each record in its own owned Kubernetes Secret, so KMS key
// material persists the way Secret Manager's payloads do and never sits in a
// file of CloudBurrow's own. Every Secret carries the ownership, instance and
// service labels, which is what /admin/reset deletes by.
type KubeStore struct {
	kube      *k8s.Runner
	namespace string
	instance  string
	mu        sync.Mutex
	// epoch, when set, labels every Secret this process writes
	// (cloudburrow.dev/kms-epoch), so an ephemeral run can delete what an
	// earlier run left without touching what it wrote itself (#481).
	epoch string
}

// epochLabel marks a Secret written by an ephemeral run.
const epochLabel = "cloudburrow.dev/kms-epoch"

// SetEpoch labels this process's writes with epoch; see Forget.
func (k *KubeStore) SetEpoch(epoch string) { k.epoch = epoch }

// Forget deletes the Secrets --mode says must not survive a restart (#481).
// With an epoch (ephemeral mode) that is every KMS Secret of this instance not
// written by this process; `!=` also matches Secrets without the label, which
// a persistent run wrote. Without one (persistent mode) it is every Secret an
// ephemeral run wrote, so its keys do not reappear in a persistent instance.
func (k *KubeStore) Forget() error {
	sel := serviceLabel + "," + k8s.InstanceLabel + "=" + k.instance + "," + epochLabel
	if k.epoch != "" {
		sel += "!=" + k.epoch
	}
	ctx, cancel := k.ctx()
	defer cancel()
	if err := k.kube.DeleteSelected(ctx, "secrets", sel, false); err != nil {
		return fmt.Errorf("delete KMS Secrets from an earlier run: %w", err)
	}
	return nil
}

// NewKubeStore returns a store over Secrets in the runner's namespace.
func NewKubeStore(kube *k8s.Runner, instance string) *KubeStore {
	return &KubeStore{kube: kube, namespace: kube.Namespace(), instance: instance}
}

const (
	serviceLabel  = "cloudburrow.dev/service=kms"
	keyAnnotation = "cloudburrow.dev/kms-key"
)

func secretName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "cb-kms-" + hex.EncodeToString(sum[:10])
}

func (k *KubeStore) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

func (k *KubeStore) Get(key string) ([]byte, error) {
	ctx, cancel := k.ctx()
	defer cancel()
	out, err := k.kube.Get(ctx, "secret", secretName(key), "jsonpath={.data.value}")
	if err != nil {
		// Only a Secret that is not there is absent. A timeout, an
		// unreachable API server or an RBAC denial is an error: taking it
		// for absence would let a create overwrite what exists.
		if k8s.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s", store.ErrNotFound, key)
		}
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", key, err)
	}
	return b, nil
}

func (k *KubeStore) Put(key string, value []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	labels := map[string]string{
		k8s.OwnedLabel: k8s.OwnedValue, k8s.InstanceLabel: k.instance,
		k8s.ServiceLabel: "kms",
	}
	if k.epoch != "" {
		labels[epochLabel] = k.epoch
	}
	manifest := map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": secretName(key), "namespace": k.namespace,
			"labels":      labels,
			"annotations": map[string]string{keyAnnotation: key},
		},
		"type": "Opaque",
		"data": map[string]string{"value": base64.StdEncoding.EncodeToString(value)},
	}
	b, _ := json.Marshal(manifest)
	ctx, cancel := k.ctx()
	defer cancel()
	if err := k.kube.Apply(ctx, string(b), k8s.ApplyOptions{}); err != nil {
		// The manifest carries the record, key material included, and
		// kubectl's stderr ends up in the error. kubectl was not seen to echo
		// a Secret's data, but it does quote an invalid field's value, so the
		// record is removed from the error in every form it could take (#390).
		return fmt.Errorf("store %s: %w", key, redact(err, value))
	}
	return nil
}

// redacted is what a removed secret reads as in an error.
const redacted = "[REDACTED]"

// longOpaque matches a run long enough to be encoded key material: base64
// (either alphabet) or hex, 32 characters or more. A truncated echo of the
// manifest would carry part of the data field, which the exact matches below
// would miss.
var longOpaque = regexp.MustCompile(`[A-Za-z0-9+/_-]{32,}={0,2}`)

// redact returns err with every occurrence of each secret removed, raw and
// base64 and hex encoded, and any long opaque run replaced. It keeps err's
// chain, so errors.Is still sees what the runner returned.
func redact(err error, secrets ...[]byte) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, sec := range secrets {
		if len(sec) == 0 {
			continue
		}
		for _, form := range []string{string(sec), base64.StdEncoding.EncodeToString(sec),
			base64.RawStdEncoding.EncodeToString(sec), base64.URLEncoding.EncodeToString(sec), hex.EncodeToString(sec)} {
			msg = strings.ReplaceAll(msg, form, redacted)
		}
	}
	msg = longOpaque.ReplaceAllString(msg, redacted)
	if msg == err.Error() {
		return err
	}
	return redactedError{msg: msg, err: err}
}

type redactedError struct {
	msg string
	err error
}

func (r redactedError) Error() string { return r.msg }

// Unwrap keeps the chain for errors.Is, not for printing: callers that print
// use Error, which is redacted.
func (r redactedError) Unwrap() error { return r.err }

func (k *KubeStore) Delete(key string) error {
	ctx, cancel := k.ctx()
	defer cancel()
	if err := k.kube.Delete(ctx, "secret", secretName(key), true); err != nil {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

func (k *KubeStore) List(prefix string) ([]string, error) {
	ctx, cancel := k.ctx()
	defer cancel()
	out, err := k.kube.List(ctx, "secrets", serviceLabel+","+k8s.InstanceLabel+"="+k.instance,
		`jsonpath={range .items[*]}{.metadata.annotations.cloudburrow\.dev/kms-key}{"\n"}{end}`)
	if err != nil {
		return nil, fmt.Errorf("list KMS records: %w", err)
	}
	var keys []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" && strings.HasPrefix(line, prefix) {
			keys = append(keys, line)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// Commit applies the writes in order. Kubernetes has no multi-object
// transaction, so a failure part-way leaves the earlier writes applied; KMS
// only commits one record at a time.
func (k *KubeStore) Commit(ops []store.Op) error {
	for _, op := range ops {
		var err error
		if op.Kind == store.OpDelete {
			err = k.Delete(op.Key)
		} else {
			err = k.Put(op.Key, op.Value)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (k *KubeStore) Close() error { return nil }

// DeleteAll removes every KMS Secret of this instance, for /admin/reset.
func (k *KubeStore) DeleteAll() error {
	ctx, cancel := k.ctx()
	defer cancel()
	return k.kube.DeleteSelected(ctx, "secrets", serviceLabel+","+k8s.InstanceLabel+"="+k.instance, false)
}
