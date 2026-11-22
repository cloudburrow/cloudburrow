package main

import (
	"context"
	"errors"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/marketplace"
)

// The Marketplace's clusters (#2149): the instance's own kind cluster and,
// when the Kubernetes Engine API is served, the project's clusters, each a
// kind cluster of its own, which the Configure page's Create a new cluster
// makes through the API as Google's does.

type marketClusters struct{ p gkeClustersProvider }

func (m marketClusters) Clusters(ctx context.Context, project string) ([]console.MarketCluster, error) {
	out := []console.MarketCluster{{Name: m.p.name, Location: "local", Status: "Running", Own: true, Kubeconfig: m.p.kubeconfig}}
	for _, c := range m.p.served(ctx, project) {
		if c.GetName() == m.p.name {
			continue
		}
		mc := console.MarketCluster{Name: c.GetName(), Location: c.GetLocation(), Status: gkeStatusText(c)}
		if gkeServedReadable(c) {
			mc.Kubeconfig = m.p.kubeconfigOf(project, c)
		}
		out = append(out, mc)
	}
	return out, nil
}

// marketNoGKE is why Create a new cluster is refused.
const marketNoGKE = "Not available locally: this instance does not serve the Kubernetes Engine API (start it with --services including gke), " +
	"so the one cluster is its own kind cluster."

func (m marketClusters) CanCreate() string {
	if m.p.gkeAPI() == nil {
		return marketNoGKE
	}
	return ""
}

// CreateCluster makes a Standard zonal cluster of one node, the smallest
// Kubernetes Engine cluster, as the Configure page's Create a new cluster
// makes one in the zone chosen.
func (m marketClusters) CreateCluster(ctx context.Context, project, zone, name string) error {
	if m.p.gkeAPI() == nil {
		return errors.New(marketNoGKE)
	}
	_, err := m.p.servedCreate(ctx, project, map[string]string{"name": name, "locationType": "Zonal", "zone": zone,
		"mode": "Standard", "nodes": "1", "machine": "e2-standard-2", "channel": "Regular channel"})
	return err
}

// marketplaceFor is the console's Marketplace: Docker Hub, deploying to
// the clusters through kubectl.
func marketplaceFor(p gkeClustersProvider) *console.Marketplace {
	return &console.Marketplace{
		Catalog:  marketplace.NewCatalog(&marketplace.DockerHub{}),
		Kube:     marketplace.Kube{Invoker: kubeInvoker},
		Clusters: marketClusters{p: p},
	}
}
