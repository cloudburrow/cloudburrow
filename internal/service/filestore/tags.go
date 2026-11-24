package filestore

import (
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Tags at creation (#2192). Instance.tags, Backup.tags and Snapshot.tags
// are "Input only. Immutable", a map whose keys are namespaced names
// (or permanent IDs) and values short names (the v1 reference, read
// 2026-11-23); https://docs.cloud.google.com/filestore/docs/tags-and-labels
// allows "a maximum of 50 tag bindings per resource". They are Resource
// Manager's tag bindings on the resource's full name,
// //file.googleapis.com/projects/P/locations/L/instances/I (backups and
// snapshots alike), so ListTagBindings and ListEffectiveTags read them,
// and Get returns no tags, the field being input only.

// FullResourcePrefix is the service part of a resource's full name.
const FullResourcePrefix = "//file.googleapis.com/"

// MaxTags is the tag bindings a resource may have.
const MaxTags = 50

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

// checkTags validates a new resource's tags before it is stored.
func (s *Server) checkTags(name string, tags map[string]string) error {
	if len(tags) == 0 {
		return nil
	}
	if len(tags) > MaxTags {
		return apierror.InvalidArgument("tags: %d tags were given; a resource can have at most %d tag bindings", len(tags), MaxTags)
	}
	b := s.tagBinder()
	if b == nil {
		return apierror.Unimplemented("tags need Resource Manager's tag bindings, which this instance does not serve; " +
			"the resource would be made without its tags, so the request is refused")
	}
	return b.BindTags(FullResourcePrefix+name, tags, true)
}

// bindTags binds a made resource's tags.
func (s *Server) bindTags(name string, tags map[string]string) {
	if len(tags) == 0 {
		return
	}
	if b := s.tagBinder(); b != nil {
		_ = b.BindTags(FullResourcePrefix+name, tags, false)
	}
}

// unbindTags drops a deleted resource's bindings.
func (s *Server) unbindTags(name string) {
	if b := s.tagBinder(); b != nil {
		_ = b.UnbindTags(FullResourcePrefix + name)
	}
}
