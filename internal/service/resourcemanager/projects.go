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
// rather than accepting it silently. One fixed organization and the folders
// under it are kept too (#1294, hierarchy.go); liens, IAM policies and
// billing are out of scope and say so.
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
	// DeleteTime is when a DELETE_REQUESTED project was deleted (RFC 3339).
	// It is purged RetentionPeriod later, until when UndeleteProject
	// restores it (#1216).
	DeleteTime string `json:"deleteTime,omitempty"`
	// Parent is "folders/{number}" or OrganizationName, or empty for a
	// top-level project, as every project was before #1294.
	Parent string `json:"parent,omitempty"`
}

// RetentionPeriod is how long a deleted project is kept in DELETE_REQUESTED
// before it is purged: Google's 30 days ("Shutting down (deleting)
// projects", cloud.google.com/resource-manager/docs/creating-managing-projects).
const RetentionPeriod = 30 * 24 * time.Hour

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
	// now is the clock deletion and purging read; a test sets it.
	now func() time.Time
}

// New returns a registry backed by st.
func New(st store.Store) *Registry { return &Registry{st: st, now: time.Now} }

// Backing is the store the registry keeps its records in, for state
// snapshots, which capture it record for record (#289).
func (r *Registry) Backing() store.Store { return r.st }

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
	if err := r.CheckParent(p.Parent); err != nil {
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

	if old, err := r.load(p.ProjectID); err == nil {
		if old.State == StateDeleteRequested {
			return Project{}, apierror.AlreadyExists(
				"project %q already exists and is pending deletion; UndeleteProject restores it", p.ProjectID)
		}
		return Project{}, apierror.AlreadyExists("project %q already exists", p.ProjectID)
	}

	p.Name = "projects/" + p.ProjectID
	p.State = StateActive
	if strings.TrimSpace(p.DisplayName) == "" {
		p.DisplayName = p.ProjectID
	}
	if p.CreateTime == "" {
		p.CreateTime = r.now().UTC().Format(time.RFC3339)
	}
	p.DeleteTime = ""

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
	p, err := r.load(id)
	if err != nil {
		return Project{}, err
	}
	if p.State == StateDeleteRequested {
		return Project{}, apierror.FailedPrecondition("project %q is pending deletion; undelete it before updating it", id)
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

	return p, r.put(p)
}

// put stores a project record.
func (r *Registry) put(p Project) error {
	encoded, err := json.Marshal(p)
	if err != nil {
		return apierror.Internal(err, "encode project")
	}
	if err := r.st.Put(key(p.ProjectID), encoded); err != nil {
		return apierror.Internal(err, "store project")
	}
	return nil
}

// load reads a project record, purging it when it has been pending deletion
// for longer than RetentionPeriod. The caller holds r.mu, or reads only.
func (r *Registry) load(id string) (Project, error) {
	b, err := r.st.Get(key(id))
	if err != nil {
		return Project{}, apierror.NotFound("project %q not found", id)
	}
	var p Project
	if err := json.Unmarshal(b, &p); err != nil {
		return Project{}, apierror.Internal(err, "decode project %q", id)
	}
	if p.State == StateDeleteRequested {
		if t, err := time.Parse(time.RFC3339, p.DeleteTime); err == nil && !r.now().Before(t.Add(RetentionPeriod)) {
			_ = r.st.Delete(key(id))
			return Project{}, apierror.NotFound("project %q not found", id)
		}
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
//
// A project pending deletion is returned, in DELETE_REQUESTED, as Google's
// GetProject returns it; one past RetentionPeriod is purged and not found.
func (r *Registry) Get(id string) (Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.load(id)
}

// List returns every project, ordered by identifier, those pending deletion
// included. Active lists the others.
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

// Active returns the projects that are not pending deletion.
func (r *Registry) Active() ([]Project, error) {
	all, err := r.List()
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, p := range all {
		if p.State != StateDeleteRequested {
			out = append(out, p)
		}
	}
	return out, nil
}

// Delete marks a project DELETE_REQUESTED, as Google's DeleteProject does
// (#1216). It is kept for RetentionPeriod, during which Undelete restores it
// and its ID cannot be used again, and is then purged.
//
// It changes the registration and nothing else. Resources created under the
// identifier live in the services that own them, and a delete here does not
// reach into those, which is stated rather than implied, because a delete
// that silently left data behind would be the worse surprise.
func (r *Registry) Delete(id string) (Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.load(id)
	if err != nil {
		return Project{}, err
	}
	if p.State == StateDeleteRequested {
		return Project{}, apierror.FailedPrecondition("project %q is already pending deletion", id)
	}
	// A lien restricting resourcemanager.projects.delete refuses it, as on
	// Google (#1295): a data rule, not authorization (ADR-0006).
	if err := r.checkDeleteLiensLocked(p.ProjectID); err != nil {
		return Project{}, err
	}
	p.State = StateDeleteRequested
	p.DeleteTime = r.now().UTC().Format(time.RFC3339)
	return p, r.put(p)
}

// Undelete restores a project pending deletion to ACTIVE.
func (r *Registry) Undelete(id string) (Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.load(id)
	if err != nil {
		return Project{}, err
	}
	if p.State != StateDeleteRequested {
		return Project{}, apierror.FailedPrecondition("project %q is not pending deletion; only a DELETE_REQUESTED project can be undeleted", id)
	}
	p.State = StateActive
	p.DeleteTime = ""
	return p, r.put(p)
}

// Move places a project under parent (#1294): a folder, the organization,
// or, empty, nowhere.
func (r *Registry) Move(id, parent string) (Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.load(id)
	if err != nil {
		return Project{}, err
	}
	if p.State == StateDeleteRequested {
		return Project{}, apierror.FailedPrecondition("project %q is pending deletion; undelete it before moving it", id)
	}
	if parent == "" {
		return Project{}, apierror.InvalidArgument("destination_parent is required")
	}
	if _, err := r.checkParentLocked(parent); err != nil {
		return Project{}, err
	}
	p.Parent = parent
	return p, r.put(p)
}

// ErrNotImplemented names a Resource Manager capability this does not provide.
func ErrNotImplemented(what string) error {
	return apierror.Unimplemented(
		"%s is not implemented: this is a local project registry, not Resource Manager. "+
			"IAM policies and billing are out of scope", what)
}
