package resourcemanager

import (
	"sort"
	"strings"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Tags given to another service's resource when it is created, such as
// Secret.tags (#1961): a map from a tag key to its value, each named by its
// permanent ID (tagKeys/N, tagValues/N) or its namespaced name, as
// https://docs.cloud.google.com/secret-manager/docs/create-and-manage-tags
// documents them; a value may also be the short name of a value of its
// key, as resources.proto's example ("123/environment": "production") has.

// resolveTagLocked is the value tags[key] names, which must be one of key's.
func (r *Registry) resolveTagLocked(key, value string) (*rmpb.TagValue, error) {
	var k *rmpb.TagKey
	var err error
	if strings.HasPrefix(key, "tagKeys/") {
		k, _, err = r.tagKeyLocked(key)
	} else {
		k, err = r.namespacedTagKeyLocked(key)
	}
	if err != nil {
		return nil, apierror.InvalidArgument("tags: tag key %q does not exist", key)
	}
	var v *rmpb.TagValue
	switch {
	case strings.HasPrefix(value, "tagValues/"):
		v, _, err = r.tagValueLocked(value)
	case strings.Contains(value, "/"):
		v, err = r.namespacedTagValueLocked(value)
	default:
		v, err = r.namespacedTagValueLocked(k.GetNamespacedName() + "/" + value)
	}
	if err != nil {
		return nil, apierror.InvalidArgument("tags: tag value %q of tag key %q does not exist", value, key)
	}
	if v.GetParent() != k.GetName() {
		return nil, apierror.InvalidArgument("tags: tag value %q (%s) is not a value of tag key %q (%s)",
			value, v.GetNamespacedName(), key, k.GetNamespacedName())
	}
	return v, nil
}

func (r *Registry) namespacedTagKeyLocked(namespaced string) (*rmpb.TagKey, error) {
	all, err := listProtos(r, tagKeyPrefix, func() *rmpb.TagKey { return &rmpb.TagKey{} })
	if err != nil {
		return nil, err
	}
	for _, k := range all {
		if k.GetNamespacedName() == namespaced {
			return k, nil
		}
	}
	return nil, apierror.NotFound("tag key %s not found", namespaced)
}

// BindTags binds tags to resource, another service's full resource name,
// as CreateTagBinding would, one binding per key; with validateOnly it
// checks every key and value and binds nothing. A key named twice (by ID
// and by name) is refused: a resource carries one value per key.
func (r *Registry) BindTags(resource string, tags map[string]string, validateOnly bool) error {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	r.mu.Lock()
	values := make([]*rmpb.TagValue, 0, len(keys))
	seen := map[string]string{}
	for _, k := range keys {
		v, err := r.resolveTagLocked(k, tags[k])
		if err != nil {
			r.unlock()
			return err
		}
		if other, ok := seen[v.GetParent()]; ok {
			r.unlock()
			return apierror.InvalidArgument("tags: %q and %q name the same tag key; a resource carries one value per key", other, k)
		}
		seen[v.GetParent()] = k
		values = append(values, v)
	}
	r.unlock()
	for _, v := range values {
		if _, err := r.CreateTagBinding(&rmpb.TagBinding{Parent: resource, TagValue: v.GetName()}, validateOnly); err != nil {
			return err
		}
	}
	return nil
}

// UnbindTags deletes every binding on resource, once it is deleted.
func (r *Registry) UnbindTags(resource string) error {
	bindings, err := r.TagBindings(resource)
	if err != nil {
		return err
	}
	for _, b := range bindings {
		if err := r.DeleteTagBinding(b.GetName()); err != nil {
			return err
		}
	}
	return nil
}
