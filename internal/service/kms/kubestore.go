package kms

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// KubeStore keeps each record in its own owned Kubernetes Secret, so KMS key
// material persists the way Secret Manager's payloads do and never sits in a
// file of CloudBurrow's own. Every Secret carries the ownership, instance and
// service labels, which is what /admin/reset deletes by.
type KubeStore struct {
	run       func(ctx context.Context, stdin string, args ...string) (string, error)
	namespace string
	instance  string
	mu        sync.Mutex
}

// Runner executes kubectl with the instance's kubeconfig.
type Runner interface {
	Run(ctx context.Context, stdin string, args ...string) (string, error)
}

// NewKubeStore returns a store over Secrets in namespace.
func NewKubeStore(r Runner, namespace, instance string) *KubeStore {
	return &KubeStore{run: r.Run, namespace: namespace, instance: instance}
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

func (k *KubeStore) kubectl(stdin string, args ...string) (string, error) {
	ctx, cancel := k.ctx()
	defer cancel()
	return k.run(ctx, stdin, append([]string{"-n", k.namespace}, args...)...)
}

func (k *KubeStore) Get(key string) ([]byte, error) {
	out, err := k.kubectl("", "get", "secret", secretName(key), "-o", "jsonpath={.data.value}")
	if err != nil {
		// Only a Secret that is not there is absent. A timeout, an
		// unreachable API server or an RBAC denial is an error: taking it
		// for absence would let a create overwrite what exists.
		if isKubectlNotFound(err) {
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
	manifest := map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": secretName(key), "namespace": k.namespace,
			"labels": map[string]string{
				"cloudburrow.dev/owned": "true", "cloudburrow.dev/instance": k.instance,
				"cloudburrow.dev/service": "kms",
			},
			"annotations": map[string]string{keyAnnotation: key},
		},
		"type": "Opaque",
		"data": map[string]string{"value": base64.StdEncoding.EncodeToString(value)},
	}
	b, _ := json.Marshal(manifest)
	if _, err := k.kubectl(string(b), "apply", "-f", "-"); err != nil {
		return fmt.Errorf("store %s: %w", key, err)
	}
	return nil
}

func (k *KubeStore) Delete(key string) error {
	if _, err := k.kubectl("", "delete", "secret", secretName(key), "--ignore-not-found"); err != nil {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

func (k *KubeStore) List(prefix string) ([]string, error) {
	out, err := k.kubectl("", "get", "secrets", "-l", serviceLabel+",cloudburrow.dev/instance="+k.instance,
		"-o", `jsonpath={range .items[*]}{.metadata.annotations.cloudburrow\.dev/kms-key}{"\n"}{end}`)
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
	_, err := k.kubectl("", "delete", "secrets", "-l", serviceLabel+",cloudburrow.dev/instance="+k.instance)
	return err
}

// isKubectlNotFound reports the API server's answer for a missing object,
// `Error from server (NotFound): secrets "x" not found`. It matches the
// status, not the words "not found", which kubectl also prints for a missing
// kubeconfig or context: that is a broken instance, not an absent key.
func isKubectlNotFound(err error) bool {
	return strings.Contains(err.Error(), "Error from server (NotFound)")
}
