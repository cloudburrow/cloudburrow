package monitoring

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/genproto/googleapis/api/monitoredres"
)

// Resource metadata (#2146). Google's groups match monitored resources by
// their metadata as well as their type and labels: the console's Name, Tag
// and Region criteria (docs.cloud.google.com/monitoring/groups, read
// 2026-11-22) are the resource's display name, its network tags and
// labels, and its location's region, and the API's group filters take
// resource.metadata.name, resource.metadata.region and
// metadata.user_labels / metadata.system_labels
// (docs.cloud.google.com/monitoring/api/v3/filters, "Resource selector for
// group definitions"). The metadata comes from each product's own
// resources, read through its API by a ResourceInventory.

// ResourceMetadata is a monitored resource's metadata.
type ResourceMetadata struct {
	// Name is the resource's display name.
	Name string
	// Region is the region of the resource's location ("global" for one
	// that has none).
	Region string
	// UserLabels are the labels the user set on the resource.
	UserLabels map[string]string
	// SystemLabels are the ones Google sets, such as gce_instance's
	// machine_type.
	SystemLabels map[string]string
	// NetworkTags are a VM's network tags.
	NetworkTags []string
}

func (m *ResourceMetadata) name() (string, bool) {
	if m == nil || m.Name == "" {
		return "", false
	}
	return m.Name, true
}

func (m *ResourceMetadata) region() (string, bool) {
	if m == nil || m.Region == "" {
		return "", false
	}
	return m.Region, true
}

func (m *ResourceMetadata) userLabels() map[string]string {
	if m == nil {
		return nil
	}
	return m.UserLabels
}

func (m *ResourceMetadata) systemLabels() map[string]string {
	if m == nil {
		return nil
	}
	return m.SystemLabels
}

// tags are what the console's Tag criterion matches: the network tags
// (with empty values) and the user labels.
func (m *ResourceMetadata) tags() map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m.NetworkTags)+len(m.UserLabels))
	for _, t := range m.NetworkTags {
		out[t] = ""
	}
	for k, v := range m.UserLabels {
		out[k] = v
	}
	return out
}

// RegionOf is the region of a zone ("us-central1-a" is "us-central1") or
// of a region; a multi-region or "global" is itself.
func RegionOf(location string) string {
	l := strings.ToLower(strings.TrimSpace(location))
	if i := strings.LastIndex(l, "-"); i > 0 && len(l)-i == 2 && l[i+1] >= 'a' && l[i+1] <= 'z' {
		return l[:i]
	}
	return l
}

// InventoryResource is one product resource as a monitored resource, with
// its metadata.
type InventoryResource struct {
	Resource *monitoredres.MonitoredResource
	Metadata ResourceMetadata
}

// ResourceInventory lists a product's resources in a project.
type ResourceInventory func(ctx context.Context, project string) ([]InventoryResource, error)

// inventoryTimeout bounds the inventories' answer to one call.
const inventoryTimeout = 15 * time.Second

// AddResourceInventory adds a product whose resources are group members.
func (s *Server) AddResourceInventory(inv ResourceInventory) {
	s.mu.Lock()
	s.inventories = append(s.inventories, inv)
	s.mu.Unlock()
}

// monResKey is a monitored resource's identity: its type and labels.
func monResKey(r *monitoredres.MonitoredResource) string {
	var b strings.Builder
	b.WriteString(r.GetType())
	writeLabels(&b, r.GetLabels())
	return b.String()
}

// inventory is a project's product resources, by resource key; a product
// that does not answer contributes none. It must be called without mu.
func (s *Server) inventory(ctx context.Context, project string) map[string]InventoryResource {
	s.mu.Lock()
	invs := append([]ResourceInventory(nil), s.inventories...)
	s.mu.Unlock()
	if len(invs) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, inventoryTimeout)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	out := map[string]InventoryResource{}
	for _, inv := range invs {
		wg.Add(1)
		go func(inv ResourceInventory) {
			defer wg.Done()
			rs, _ := inv(ctx, project)
			mu.Lock()
			defer mu.Unlock()
			for _, r := range rs {
				if r.Resource != nil {
					out[monResKey(r.Resource)] = r
				}
			}
		}(inv)
	}
	wg.Wait()
	return out
}

// ResourceMetadataIndex looks up monitored resources' metadata among the
// project's inventories (nil for a resource none lists); for the console.
func (s *Server) ResourceMetadataIndex(ctx context.Context, project string) func(*monitoredres.MonitoredResource) *ResourceMetadata {
	inv := s.inventory(ctx, project)
	return func(r *monitoredres.MonitoredResource) *ResourceMetadata {
		if r == nil {
			return nil
		}
		return metaOf(inv, r)
	}
}

// inventoryKeys are an inventory's keys in order.
func inventoryKeys(inv map[string]InventoryResource) []string {
	keys := make([]string, 0, len(inv))
	for k := range inv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
