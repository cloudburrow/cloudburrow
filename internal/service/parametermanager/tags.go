package parametermanager

import "github.com/cloudburrow/cloudburrow/internal/apierror"

// Tags at creation (#2063). Parameter.tags is "Input only. Immutable. Tag
// keys and tag values that are bound to this Parameter" (service.proto),
// each key by its namespaced name ("123/environment") or permanent ID, each
// value by its short name, namespaced name or ID; "Create and manage tags"
// (docs.cloud.google.com/secret-manager/parameter-manager/docs/
// create-and-manage-tags, read 2026-11-20) binds them to the parameter's
// full resource name,
// //parametermanager.googleapis.com/projects/PROJECT_ID/locations/LOCATION/parameters/PARAMETER_ID,
// global and regional alike. The bindings are Resource Manager's, as a
// secret's are (#1961): its ListTagBindings and ListEffectiveTags read them,
// CreateTagBinding and DeleteTagBinding change them, and GetParameter
// returns no tags, the field being input only. Deleting the parameter
// deletes its bindings.

// FullResourcePrefix is the service part of a parameter's full resource
// name.
const FullResourcePrefix = "//parametermanager.googleapis.com/"

// TagBinder binds tags through Resource Manager.
type TagBinder interface {
	// BindTags binds each key's value to resource, a full resource name;
	// with validateOnly it only checks that it could.
	BindTags(resource string, tags map[string]string, validateOnly bool) error
	// UnbindTags removes every binding on resource.
	UnbindTags(resource string) error
}

func (s *Server) tagBinder() TagBinder {
	if s.opts.Tags == nil {
		return nil
	}
	return s.opts.Tags()
}

// checkTags validates a new parameter's tags before it is stored.
func (s *Server) checkTags(name string, tags map[string]string) error {
	if len(tags) == 0 {
		return nil
	}
	b := s.tagBinder()
	if b == nil {
		return apierror.Unimplemented("Parameter.tags needs Resource Manager's tag bindings, which this instance does not serve; " +
			"the parameter would be stored without its tags, so the request is refused")
	}
	return b.BindTags(FullResourcePrefix+name, tags, true)
}

// bindTags binds a stored parameter's tags.
func (s *Server) bindTags(name string, tags map[string]string) error {
	if len(tags) == 0 {
		return nil
	}
	b := s.tagBinder()
	if b == nil {
		return apierror.Unimplemented("Parameter.tags needs Resource Manager's tag bindings")
	}
	return b.BindTags(FullResourcePrefix+name, tags, false)
}

// unbindTags drops a deleted parameter's bindings.
func (s *Server) unbindTags(name string) {
	if b := s.tagBinder(); b != nil {
		_ = b.UnbindTags(FullResourcePrefix + name)
	}
}
