package marketplacetest

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/kube"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/releaseutil"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	"sigs.k8s.io/yaml"

	"github.com/cloudburrow/cloudburrow/internal/marketplace"
)

// Helm is the Helm SDK over the in-memory Cluster: releases are kept in
// memory, and a release's objects are applied to and deleted from the
// Cluster as kubectl would, so its components read back as they would on
// a cluster.
type Helm struct {
	C     *Cluster
	mu    sync.Mutex
	store *driver.Memory
}

// NewHelm is a Helm over c.
func NewHelm(c *Cluster) *Helm { return &Helm{C: c, store: driver.NewMemory()} }

// Config is a marketplace.HelmConfigFunc.
func (h *Helm) Config() marketplace.HelmConfigFunc {
	return func(_, namespace string) (*action.Configuration, error) {
		h.mu.Lock()
		h.store.SetNamespace(namespace)
		h.mu.Unlock()
		return &action.Configuration{Releases: storage.Init(h.store), KubeClient: &helmClient{h: h, ns: namespace},
			Capabilities: chartutil.DefaultCapabilities, Log: func(string, ...any) {}}, nil
	}
}

// helmClient keeps the manifests built, and Create, Update and Delete act
// on them: Helm builds manifests (an install the release's and its
// namespace's), then creates every one built, or updates to or deletes the
// last one built.
type helmClient struct {
	kubefake.PrintingKubeClient
	h     *Helm
	ns    string
	built []string
}

func (c *helmClient) Build(r io.Reader, _ bool) (kube.ResourceList, error) {
	b, err := io.ReadAll(r)
	c.built = append(c.built, string(b))
	return kube.ResourceList{}, err
}

func (c *helmClient) each(verb string, all bool) error {
	manifests := c.built
	if !all && len(manifests) > 0 {
		manifests = manifests[len(manifests)-1:]
	}
	c.built = nil
	for _, m := range releaseutil.SplitManifests(strings.Join(manifests, "\n---\n")) {
		j, err := yaml.YAMLToJSON([]byte(m))
		if err != nil {
			return err
		}
		var head struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if yaml.Unmarshal(j, &head) != nil || head.Kind == "" {
			continue
		}
		if verb == "apply" {
			_, err = c.h.C.Run(context.Background(), string(j), "apply", "-n", c.ns, "-f", "-")
		} else {
			_, err = c.h.C.Run(context.Background(), "", "delete", head.Kind, head.Metadata.Name, "-n", c.ns, "--ignore-not-found")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *helmClient) Create(kube.ResourceList) (*kube.Result, error) {
	return &kube.Result{}, c.each("apply", true)
}

func (c *helmClient) Update(_, _ kube.ResourceList, _ bool) (*kube.Result, error) {
	return &kube.Result{}, c.each("apply", false)
}

func (c *helmClient) Delete(kube.ResourceList) (*kube.Result, []error) {
	if err := c.each("delete", false); err != nil {
		return nil, []error{err}
	}
	return &kube.Result{}, nil
}

func (c *helmClient) Wait(kube.ResourceList, time.Duration) error { return nil }
