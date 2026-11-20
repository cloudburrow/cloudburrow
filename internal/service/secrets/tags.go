package secrets

import (
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Tags at creation (#1961). Secret.tags is "Input only. Immutable. Mapping
// of Tag keys/values directly bound to this resource" (resources.proto), and
// https://docs.cloud.google.com/secret-manager/docs/create-and-manage-tags
// names each key and value by its permanent ID (tagKeys/N, tagValues/N) or
// its namespaced name, and the secret's bindings by its full resource name,
// //secretmanager.googleapis.com/projects/PROJECT_ID/secrets/SECRET_ID. The
// bindings are Resource Manager's: the instance's registry keeps them, so
// ListTagBindings and ListEffectiveTags read them, and GetSecret returns no
// tags, the field being input only.

// FullResourcePrefix is the service part of a secret's full resource name.
const FullResourcePrefix = "//secretmanager.googleapis.com/"

// TagBinder binds tags through Resource Manager.
type TagBinder interface {
	// BindTags binds each key's value to resource, a full resource name;
	// with validateOnly it only checks that it could.
	BindTags(resource string, tags map[string]string, validateOnly bool) error
	// UnbindTags removes every binding on resource.
	UnbindTags(resource string) error
}

// SetTagBinder sets Resource Manager's tag bindings, a function since the
// registry opens after Secret Manager starts. With none, a secret with
// tags is refused UNIMPLEMENTED, since nothing could bind them.
func (s *Store) SetTagBinder(b func() TagBinder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tags = b
}

func (s *Store) tagBinder() TagBinder {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tags == nil {
		return nil
	}
	return s.tags()
}

// CheckTags validates a new secret's tags against Resource Manager before
// the secret named name is stored.
func (s *Store) CheckTags(name string, tags map[string]string) error {
	if len(tags) == 0 {
		return nil
	}
	b := s.tagBinder()
	if b == nil {
		return apierror.Unimplemented("Secret.tags needs Resource Manager's tag bindings, which this instance does not serve; " +
			"the secret would be stored without its tags, so the request is refused")
	}
	return b.BindTags(FullResourcePrefix+name, tags, true)
}

// BindTags binds a stored secret's tags.
func (s *Store) BindTags(name string, tags map[string]string) error {
	if len(tags) == 0 {
		return nil
	}
	b := s.tagBinder()
	if b == nil {
		return apierror.Unimplemented("Secret.tags needs Resource Manager's tag bindings")
	}
	return b.BindTags(FullResourcePrefix+name, tags, false)
}

// unbindTagsLocked drops a deleted secret's bindings. The caller holds
// s.mu.
func (s *Store) unbindTagsLocked(name string) {
	if s.tags == nil {
		return
	}
	if b := s.tags(); b != nil {
		_ = b.UnbindTags(FullResourcePrefix + name)
	}
}
