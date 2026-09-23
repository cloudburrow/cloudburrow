// Package resourcemanager keeps the set of projects an instance knows about.
//
// Until this existed the console discovered projects by scanning resources
// that happened to exist, with a comment explaining that CloudBurrow kept no
// registry and that inventing one would be a second store. That was a
// defensible position when nothing could create a project — and it made the
// console unable to offer what every Cloud console offers first: choosing a
// project, and making a new one.
//
// This is a registry, not a reimplementation of Resource Manager. It holds
// what a local instance needs — the identifier, a display name, lifecycle
// state and creation time — and refuses everything it does not implement
// rather than accepting it silently. Organisations, folders, liens, IAM
// policies and billing are out of scope and say so.
package resourcemanager

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// State is a project's lifecycle state, spelled as Resource Manager spells it.
type State string

const (
	StateActive          State = "ACTIVE"
	StateDeleteRequested State = "DELETE_REQUESTED"
)

// Project is one project.
type Project struct {
	// Name is the resource name, "projects/{projectId}".
	Name string `json:"name"`
	// ProjectID is the identifier clients use everywhere else.
	ProjectID string `json:"projectId"`
	// DisplayName is the human-facing name; it defaults to the identifier.
	DisplayName string `json:"displayName"`
	State       State  `json:"state"`
	CreateTime  string `json:"createTime"`
	// Labels are stored and returned unchanged. Nothing interprets them.
	Labels map[string]string `json:"labels,omitempty"`
}

// projectIDPattern is Google's rule for a project identifier: 6 to 30
// characters, lowercase letters, digits and hyphens, starting with a letter
// and not ending with one.
//
// It is enforced here rather than left to the UI so that a project created
// through any path is one the real API would also have accepted.
var projectIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

// ValidateProjectID reports why an identifier is unacceptable, or nil.
func ValidateProjectID(id string) error {
	switch {
	case id == "":
		return apierror.InvalidArgument("a project ID is required")
	case len(id) < 6 || len(id) > 30:
		return apierror.InvalidArgument(
			"project ID %q must be between 6 and 30 characters, got %d", id, len(id))
	case !projectIDPattern.MatchString(id):
		return apierror.InvalidArgument(
			"project ID %q must start with a lowercase letter, contain only lowercase "+
				"letters, digits and hyphens, and not end with a hyphen", id)
	}
	return nil
}

const keyPrefix = "resourcemanager/projects/"

// Registry stores projects.
type Registry struct {
	mu sync.Mutex
	st store.Store
}

// New returns a registry backed by st.
func New(st store.Store) *Registry { return &Registry{st: st} }

func key(id string) string { return keyPrefix + id }

// Create adds a project, enforcing the identifier rules.
//
// This is the path a person takes, so it validates: a project created here
// should be one the real API would also have accepted, or it will fail the
// first time someone tries to create it for real.
func (r *Registry) Create(p Project) (Project, error) {
	if err := ValidateProjectID(p.ProjectID); err != nil {
		return Project{}, err
	}
	return r.register(p)
}

// register stores a project without applying the creation rules.
func (r *Registry) register(p Project) (Project, error) {
	if strings.TrimSpace(p.ProjectID) == "" {
		return Project{}, apierror.InvalidArgument("a project ID is required")
	}
	// A slash would split the resource name "projects/{id}" into something
	// that no longer round-trips, and it is a path separator in the store's
	// key space, which SafeKey permits because keys are path-like.
	if strings.ContainsAny(p.ProjectID, "/\\") {
		return Project{}, apierror.InvalidArgument(
			"project ID %q cannot contain a path separator", p.ProjectID)
	}
	if !store.SafeKey(key(p.ProjectID)) {
		return Project{}, apierror.InvalidArgument(
			"project ID %q cannot be stored", p.ProjectID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, err := r.st.Get(key(p.ProjectID)); err == nil {
		return Project{}, apierror.AlreadyExists("project %q already exists", p.ProjectID)
	}

	p.Name = "projects/" + p.ProjectID
	p.State = StateActive
	if strings.TrimSpace(p.DisplayName) == "" {
		p.DisplayName = p.ProjectID
	}
	if p.CreateTime == "" {
		p.CreateTime = time.Now().UTC().Format(time.RFC3339)
	}

	b, err := json.Marshal(p)
	if err != nil {
		return Project{}, apierror.Internal(err, "encode project")
	}
	if err := r.st.Put(key(p.ProjectID), b); err != nil {
		return Project{}, apierror.Internal(err, "store project")
	}
	return p, nil
}

// Update changes a project's display name and labels.
//
// The identifier, the state and the creation time are not changeable: the first
// is the project's identity, and the other two are facts about what happened
// rather than settings. Google's UpdateProject is the same shape — it accepts a
// field mask over displayName and labels and nothing else.
func (r *Registry) Update(id, displayName string, labels map[string]string) (Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Read under the same lock as the write. Reading through Get would drop the
	// lock between the two, so two concurrent updates could each write a project
	// built from the state before the other.
	b, err := r.st.Get(key(id))
	if err != nil {
		return Project{}, apierror.NotFound("project %q not found", id)
	}
	var p Project
	if err := json.Unmarshal(b, &p); err != nil {
		return Project{}, apierror.Internal(err, "decode project %q", id)
	}

	if name := strings.TrimSpace(displayName); name != "" {
		p.DisplayName = name
	} else {
		// Cleared rather than left alone, because the display name defaults to
		// the identifier and "no display name" is not a state a project has.
		p.DisplayName = p.ProjectID
	}
	if len(labels) == 0 {
		p.Labels = nil
	} else {
		p.Labels = labels
	}

	encoded, err := json.Marshal(p)
	if err != nil {
		return Project{}, apierror.Internal(err, "encode project")
	}
	if err := r.st.Put(key(id), encoded); err != nil {
		return Project{}, apierror.Internal(err, "store project")
	}
	return p, nil
}

// EnsureExists registers a project when it is absent, and is a no-op
// otherwise.
//
// The instance's own project is registered this way at startup, so the console
// always has one to select and a fresh instance is never projectless.
//
// It deliberately does *not* apply the creation rules. The identifier already
// exists — an instance named "demo" has been answering for project "demo"
// since it started, and resources may already sit under it. Google validates
// when a project is created, not when one is referred to, and refusing to
// register a name already in use left the registry empty and the picker with
// nothing to show. Creating a new project from the console still validates.
func (r *Registry) EnsureExists(id string) (Project, error) {
	if p, err := r.Get(id); err == nil {
		return p, nil
	}
	return r.register(Project{ProjectID: id})
}

// Get returns one project.
func (r *Registry) Get(id string) (Project, error) {
	b, err := r.st.Get(key(id))
	if err != nil {
		return Project{}, apierror.NotFound("project %q not found", id)
	}
	var p Project
	if err := json.Unmarshal(b, &p); err != nil {
		return Project{}, apierror.Internal(err, "decode project %q", id)
	}
	return p, nil
}

// List returns every project, ordered by identifier.
func (r *Registry) List() ([]Project, error) {
	keys, err := r.st.List(keyPrefix)
	if err != nil {
		return nil, apierror.Internal(err, "list projects")
	}
	out := make([]Project, 0, len(keys))
	for _, k := range keys {
		id := strings.TrimPrefix(k, keyPrefix)
		p, err := r.Get(id)
		if err != nil {
			continue // a key that will not decode is skipped, not fatal
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProjectID < out[j].ProjectID })
	return out, nil
}

// Delete removes a project from the registry.
//
// It deletes the registration and nothing else. Resources created under the
// identifier live in the services that own them, and removing a row here does
// not reach into those — which is stated rather than implied, because a delete
// that silently left data behind would be the worse surprise.
func (r *Registry) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.st.Get(key(id)); err != nil {
		return apierror.NotFound("project %q not found", id)
	}
	if err := r.st.Delete(key(id)); err != nil {
		return apierror.Internal(err, "delete project %q", id)
	}
	return nil
}

// ErrNotImplemented names a Resource Manager capability this does not provide.
func ErrNotImplemented(what string) error {
	return apierror.Unimplemented(
		"%s is not implemented: this is a local project registry, not Resource Manager. "+
			"Organisations, folders, liens, IAM policies and billing are out of scope", what)
}
