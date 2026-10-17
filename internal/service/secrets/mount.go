package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// Mounting a version under a customer-managed key (#1489).
//
// A Cloud Run secretKeyRef or secret volume reads a Kubernetes Secret, and
// the Secret that stores a version under a customer-managed key (#1453)
// holds the sealed payload. So when a revision references such a version,
// the instance decrypts it, through its own Cloud KMS exactly as
// AccessSecretVersion does, and writes the plaintext to a second Kubernetes
// Secret, the mount Secret, which the pod references. The pod is never given
// a key.
//
// docs.cloud.google.com/run/docs/configuring/services/secrets documents that
// a secret Cloud Run cannot access stops the revision from deploying, and
// that an environment variable is read when an instance starts; a key that
// cannot decrypt makes the version inaccessible
// (docs.cloud.google.com/secret-manager/docs/cmek). So:
//
//   - a deployment that references a version whose key cannot decrypt is
//     refused FAILED_PRECONDITION naming the key, as an access is;
//   - while it is deployed, RefreshMounts re-checks every mounted version
//     every mountRefresh: when the key is disabled or destroyed, or the
//     version disabled or destroyed, the plaintext is removed from the mount
//     Secret, so an instance that starts then cannot (Kubernetes reports
//     the missing key); instances already running keep what they read, as
//     on Cloud Run. When the key can decrypt again the plaintext is put back.
//
// The mount Secret goes with the secret: DeleteSecret, an expiry and
// /admin/reset delete it.

// mountRefresh is how often RefreshMounts re-checks the mounted versions.
const mountRefresh = 10 * time.Second

// Mount is one version a workload references through a mount Secret.
type Mount struct {
	Project, ID string
	Number      int
	// Present is whether the mount Secret holds its plaintext now.
	Present bool
}

// mounter is a store that can hold the decrypted copy of a version for a
// workload to mount; KubeStore does, in the mount Secret.
type mounter interface {
	// SetMount records number as mounted and writes its plaintext, or, with
	// nil, removes the plaintext but keeps it recorded.
	SetMount(project, id string, number int, plaintext []byte) error
	// DeleteMounts deletes a secret's mount Secret.
	DeleteMounts(project, id string) error
	// Mounts lists every recorded version.
	Mounts() ([]Mount, error)
}

// MountSecretName is the Kubernetes Secret holding the decrypted copies of
// a secret's versions under a customer-managed key that workloads mount.
func MountSecretName(project, secret string) string {
	return KubernetesSecretName(project, secret) + "-plain"
}

// resolveEncrypted decrypts v for a workload and writes it to the mount
// Secret. The caller holds s.mu.
func (s *Store) resolveEncrypted(project, id string, v Version) (string, string, error) {
	m, ok := s.db.(mounter)
	if !ok {
		return "", "", apierror.Unimplemented("secret version %s is encrypted with a customer-managed key, and "+
			"mounting it needs the cluster this instance has not got", v.Name)
	}
	open, err := s.openVersion(v)
	if err != nil {
		return "", "", err
	}
	if err := m.SetMount(project, id, v.Number, open.Payload); err != nil {
		return "", "", apierror.Internal(err, "write the decrypted copy of %s for mounting", v.Name)
	}
	return MountSecretName(project, id), VersionKey(v.Number), nil
}

// dropMount removes v's plaintext from its mount Secret, if it has one. The
// caller holds s.mu.
func (s *Store) dropMount(project, id string, v Version) error {
	m, ok := s.db.(mounter)
	if !ok || !v.encrypted() {
		return nil
	}
	mounts, err := m.Mounts()
	if err != nil {
		return apierror.Internal(err, "list mounted secret versions")
	}
	for _, mt := range mounts {
		if mt.Project == project && mt.ID == id && mt.Number == v.Number && mt.Present {
			if err := m.SetMount(project, id, v.Number, nil); err != nil {
				return apierror.Internal(err, "remove the decrypted copy of %s", v.Name)
			}
		}
	}
	return nil
}

// deleteMounts deletes a secret's mount Secret. The caller holds s.mu.
func (s *Store) deleteMounts(project, id string) error {
	if m, ok := s.db.(mounter); ok {
		if err := m.DeleteMounts(project, id); err != nil {
			return apierror.Internal(err, "delete the mount Secret of %s", SecretName(project, id))
		}
	}
	return nil
}

// RefreshMounts re-checks every mounted version: its plaintext is kept only
// while the version is enabled and its keys can decrypt it.
func (s *Store) RefreshMounts() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.db.(mounter)
	if !ok {
		return nil
	}
	mounts, err := m.Mounts()
	if err != nil {
		return err
	}
	for _, mt := range mounts {
		// A version that is gone (or cannot be read) keeps no plaintext.
		var plaintext []byte
		if v, err := s.getVersion(mt.Project, mt.ID, mt.Number); err == nil && v.Accessible() && v.encrypted() {
			if open, err := s.openVersion(v); err == nil {
				plaintext = open.Payload
			}
		}
		var err error
		switch {
		case plaintext == nil && mt.Present:
			err = m.SetMount(mt.Project, mt.ID, mt.Number, nil)
		case plaintext != nil && !mt.Present:
			err = m.SetMount(mt.Project, mt.ID, mt.Number, plaintext)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// runMounts calls RefreshMounts every mountRefresh until ctx ends.
func (s *Store) runMounts(ctx context.Context) {
	if _, ok := s.db.(mounter); !ok {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.clock.After(mountRefresh):
		}
		// A failure (the cluster not up yet) is retried on the next tick.
		_ = s.RefreshMounts()
	}
}

// --- KubeStore -----------------------------------------------------------

const (
	// MountLabel marks a mount Secret.
	MountLabel = "cloudburrow.dev/secret-mount"
	// mountProjectAnnotation and mountIDAnnotation name the secret a mount
	// Secret belongs to; they are not the annotations listAll reads, so it
	// is never taken for a secret.
	mountProjectAnnotation = "cloudburrow.dev/mount-project"
	mountIDAnnotation      = "cloudburrow.dev/mount-secret"
	// mountVersionsAnnotation lists the mounted version numbers, present
	// or not.
	mountVersionsAnnotation = "cloudburrow.dev/mount-versions"
)

var _ mounter = (*KubeStore)(nil)

// SetMount writes a version's plaintext to the mount Secret, or removes it.
func (k *KubeStore) SetMount(project, id string, number int, plaintext []byte) error {
	ctx := context.Background()
	k.mu.Lock()
	defer k.mu.Unlock()

	name := MountSecretName(project, id)
	ks := kubeSecret{}
	out, err := k.kube.Get(ctx, "secret", name, "json")
	switch {
	case err == nil:
		if err := json.Unmarshal([]byte(out), &ks); err != nil {
			return fmt.Errorf("decode secret %s: %w", name, err)
		}
	case !k8s.IsNotFound(err):
		return err
	}
	if ks.Data == nil {
		ks.Data = map[string]string{}
	}
	numbers := mountNumbers(ks.Metadata.Annotations[mountVersionsAnnotation])
	numbers[number] = true
	if plaintext != nil {
		ks.Data[VersionKey(number)] = base64.StdEncoding.EncodeToString(plaintext)
	} else {
		delete(ks.Data, VersionKey(number))
	}

	labels := map[string]string{
		OwnerLabel:   k8s.OwnedValue,
		ServiceLabel: ServiceLabelValue,
		ProjectLabel: labelSafe(project),
		MountLabel:   "true",
	}
	if k.instance != "" {
		labels[k8s.InstanceLabel] = labelSafe(k.instance)
	}
	if k.epoch != "" {
		labels[EpochLabel] = k.epoch
	}
	list := make([]int, 0, len(numbers))
	for n := range numbers {
		list = append(list, n)
	}
	sort.Ints(list)
	strs := make([]string, len(list))
	for i, n := range list {
		strs[i] = strconv.Itoa(n)
	}
	manifest, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "Opaque",
		"metadata": map[string]any{
			"name":      name,
			"namespace": k.namespace,
			"labels":    labels,
			"annotations": map[string]string{
				mountProjectAnnotation:  project,
				mountIDAnnotation:       id,
				mountVersionsAnnotation: strings.Join(strs, ","),
			},
		},
		"data": ks.Data,
	})
	if err != nil {
		return fmt.Errorf("encode mount secret manifest: %w", err)
	}
	// Server-side, for the reason apply gives: no annotation copy of data.
	return k.kube.Apply(ctx, string(manifest), k8s.ApplyOptions{
		ServerSide: true, ForceConflicts: true, FieldManager: "cloudburrow-secretmanager",
	})
}

// DeleteMounts deletes a secret's mount Secret.
func (k *KubeStore) DeleteMounts(project, id string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.kube.Delete(context.Background(), "secret", MountSecretName(project, id), true)
}

// Mounts lists every version recorded in a mount Secret.
func (k *KubeStore) Mounts() ([]Mount, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	selector := k8s.OwnedSelector + "," + MountLabel + "=true"
	out, err := k.kube.List(context.Background(), "secret", selector, "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []kubeSecret `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("decode secret list: %w", err)
	}
	var mounts []Mount
	for _, ks := range list.Items {
		project, id := ks.Metadata.Annotations[mountProjectAnnotation], ks.Metadata.Annotations[mountIDAnnotation]
		if project == "" || id == "" || ks.Metadata.Name != MountSecretName(project, id) {
			continue
		}
		numbers := mountNumbers(ks.Metadata.Annotations[mountVersionsAnnotation])
		list := make([]int, 0, len(numbers))
		for n := range numbers {
			list = append(list, n)
		}
		sort.Ints(list)
		for _, n := range list {
			_, present := ks.Data[VersionKey(n)]
			mounts = append(mounts, Mount{Project: project, ID: id, Number: n, Present: present})
		}
	}
	return mounts, nil
}

// mountNumbers parses mountVersionsAnnotation.
func mountNumbers(s string) map[int]bool {
	out := map[int]bool{}
	for _, f := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil {
			out[n] = true
		}
	}
	return out
}
