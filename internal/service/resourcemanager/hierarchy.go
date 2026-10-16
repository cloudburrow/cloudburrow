package resourcemanager

// The resource hierarchy (#1294): one fixed local organization, and folders
// under it or under other folders, which projects may be parented by.
//
// The organization is not created or deleted: an instance has exactly one,
// organizations/OrganizationNumber, named OrganizationDomain, always ACTIVE.
// Folders are stored in the registry's store, numbered at random as Google
// numbers them, and follow Google's rules: a display name of 1 to 30
// letters, digits, spaces, hyphens and underscores, starting and ending
// with a letter or digit, unique among its active siblings; at most
// MaxFolderDepth folders deep; deleted only when empty, and then kept
// DELETE_REQUESTED for RetentionPeriod, when UndeleteFolder restores it.
//
// Policies stay stored, not enforced (ADR-0006), and folder and organization
// IAM is UNIMPLEMENTED for the reason project IAM is.

import (
	"crypto/rand"
	"encoding/json"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

const (
	// OrganizationNumber is the local organization's number. It is fixed, so
	// a module or script that names organizations/100000000001 works on every
	// instance.
	OrganizationNumber = "100000000001"
	// OrganizationName is its resource name.
	OrganizationName = "organizations/" + OrganizationNumber
	// OrganizationDomain is its display name, the domain Google shows for an
	// organization.
	OrganizationDomain = "cloudburrow.local"
	// OrganizationCustomerID is its Google Workspace directory customer ID.
	OrganizationCustomerID = "C0cloudburrow"
	// OrganizationCreateTime is when it "was created": a fixed instant, so
	// every read agrees.
	OrganizationCreateTime = "2024-01-01T00:00:00Z"

	// MaxFolderDepth is Google's limit on nested folders.
	MaxFolderDepth = 10
)

const folderPrefix = "resourcemanager/folders/"

// Folder is one folder.
type Folder struct {
	// Name is "folders/{number}".
	Name string `json:"name"`
	// Parent is "folders/{number}" or OrganizationName.
	Parent      string `json:"parent"`
	DisplayName string `json:"displayName"`
	State       State  `json:"state"`
	CreateTime  string `json:"createTime"`
	UpdateTime  string `json:"updateTime"`
	DeleteTime  string `json:"deleteTime,omitempty"`
}

// ID is the folder's number.
func (f Folder) ID() string { return strings.TrimPrefix(f.Name, "folders/") }

// Etag is a digest of the folder's state.
func (f Folder) Etag() string {
	b, _ := json.Marshal(f)
	return policyEtag(b)
}

var folderNamePattern = regexp.MustCompile(`^[\p{L}\p{N}]([\p{L}\p{N} _-]{0,28}[\p{L}\p{N}])?$`)

// ValidateFolderName reports why a display name is unacceptable, or nil.
func ValidateFolderName(name string) error {
	if !folderNamePattern.MatchString(name) {
		return apierror.InvalidArgument("folder display name %q must be 1 to 30 letters, digits, spaces, hyphens and underscores, "+
			"starting and ending with a letter or digit", name)
	}
	return nil
}

func folderKey(id string) string { return folderPrefix + id }

// FolderName normalizes "folders/{n}" or "{n}" to "folders/{n}".
func FolderName(s string) (string, error) {
	id := strings.TrimPrefix(s, "folders/")
	if _, err := strconv.ParseUint(id, 10, 64); err != nil || id == "" {
		return "", apierror.InvalidArgument("folder %q must be folders/{folder_number}", s)
	}
	return "folders/" + id, nil
}

// loadFolder reads a folder, purging it when it has been pending deletion for
// longer than RetentionPeriod. The caller holds r.mu.
func (r *Registry) loadFolder(name string) (Folder, error) {
	n, err := FolderName(name)
	if err != nil {
		return Folder{}, err
	}
	id := strings.TrimPrefix(n, "folders/")
	b, err := r.st.Get(folderKey(id))
	if err != nil {
		return Folder{}, apierror.NotFound("folder %s not found", n)
	}
	var f Folder
	if err := json.Unmarshal(b, &f); err != nil {
		return Folder{}, apierror.Internal(err, "decode folder %s", n)
	}
	if f.State == StateDeleteRequested {
		if t, err := time.Parse(time.RFC3339, f.DeleteTime); err == nil && !r.now().Before(t.Add(RetentionPeriod)) {
			_ = r.st.Delete(folderKey(id))
			r.dropIAM(n)
			return Folder{}, apierror.NotFound("folder %s not found", n)
		}
	}
	return f, nil
}

func (r *Registry) putFolder(f Folder) error {
	b, err := json.Marshal(f)
	if err != nil {
		return apierror.Internal(err, "encode folder")
	}
	if err := r.st.Put(folderKey(f.ID()), b); err != nil {
		return apierror.Internal(err, "store folder")
	}
	return nil
}

// folders lists every stored folder. The caller holds r.mu.
func (r *Registry) folders() ([]Folder, error) {
	keys, err := r.st.List(folderPrefix)
	if err != nil {
		return nil, apierror.Internal(err, "list folders")
	}
	out := make([]Folder, 0, len(keys))
	for _, k := range keys {
		f, err := r.loadFolder(strings.TrimPrefix(k, folderPrefix))
		if err != nil {
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Folders returns every folder, those pending deletion included.
func (r *Registry) Folders() ([]Folder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.folders()
}

// Folder returns one folder.
func (r *Registry) Folder(name string) (Folder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loadFolder(name)
}

// checkParentLocked validates a parent a folder or project is placed under:
// the organization, or an ACTIVE folder. It returns the parent's depth (0
// for the organization). The caller holds r.mu.
func (r *Registry) checkParentLocked(parent string) (int, error) {
	if parent == OrganizationName {
		return 0, nil
	}
	if strings.HasPrefix(parent, "organizations/") {
		return 0, apierror.NotFound("organization %s not found; the local organization is %s", parent, OrganizationName)
	}
	if !strings.HasPrefix(parent, "folders/") {
		return 0, apierror.InvalidArgument("parent %q must be folders/{folder_number} or organizations/{organization_number}", parent)
	}
	depth := 0
	for p := parent; strings.HasPrefix(p, "folders/"); depth++ {
		f, err := r.loadFolder(p)
		if err != nil {
			return 0, err
		}
		if p == parent && f.State != StateActive {
			return 0, apierror.FailedPrecondition("parent %s is pending deletion", parent)
		}
		if depth > MaxFolderDepth+1 {
			return 0, apierror.Internal(nil, "folder hierarchy above %s is too deep", parent)
		}
		p = f.Parent
	}
	return depth, nil
}

// CheckParent validates a parent a project may be created under or moved to.
// An empty parent is a top-level project.
func (r *Registry) CheckParent(parent string) error {
	if parent == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.checkParentLocked(parent)
	return err
}

// subtreeDepth is how many folders deep the subtree rooted at name goes,
// counting name itself. The caller holds r.mu.
func subtreeDepth(all []Folder, name string) int {
	d := 0
	for _, f := range all {
		if f.Parent == name && f.State == StateActive {
			if c := subtreeDepth(all, f.Name); c > d {
				d = c
			}
		}
	}
	return d + 1
}

func (r *Registry) checkSiblingName(all []Folder, parent, display, self string) error {
	for _, f := range all {
		if f.Parent == parent && f.State == StateActive && f.Name != self && f.DisplayName == display {
			// UNVERIFIED: Google answers a duplicate sibling name
			// FAILED_PRECONDITION (FOLDER_NAME_UNIQUENESS_VIOLATION).
			return apierror.FailedPrecondition("a folder named %q already exists under %s", display, parent)
		}
	}
	return nil
}

func randomFolderNumber() string {
	// Google's folder numbers are 12 digits, not starting with 0.
	n, err := rand.Int(rand.Reader, big.NewInt(900_000_000_000))
	if err != nil {
		return strconv.FormatInt(time.Now().UnixNano()%900_000_000_000+100_000_000_000, 10)
	}
	return strconv.FormatInt(n.Int64()+100_000_000_000, 10)
}

// CreateFolder adds a folder under parent.
func (r *Registry) CreateFolder(parent, display string) (Folder, error) {
	if err := ValidateFolderName(display); err != nil {
		return Folder{}, err
	}
	if parent == "" {
		return Folder{}, apierror.InvalidArgument("parent is required: folders/{folder_number} or %s", OrganizationName)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	depth, err := r.checkParentLocked(parent)
	if err != nil {
		return Folder{}, err
	}
	if depth+1 > MaxFolderDepth {
		return Folder{}, apierror.FailedPrecondition("folders can be nested at most %d deep", MaxFolderDepth)
	}
	all, err := r.folders()
	if err != nil {
		return Folder{}, err
	}
	if err := r.checkSiblingName(all, parent, display, ""); err != nil {
		return Folder{}, err
	}
	id := randomFolderNumber()
	for {
		if _, err := r.st.Get(folderKey(id)); err != nil {
			break
		}
		id = randomFolderNumber()
	}
	now := r.now().UTC().Format(time.RFC3339)
	f := Folder{Name: "folders/" + id, Parent: parent, DisplayName: display, State: StateActive, CreateTime: now, UpdateTime: now}
	return f, r.putFolder(f)
}

// RenameFolder changes a folder's display name.
func (r *Registry) RenameFolder(name, display string) (Folder, error) {
	if err := ValidateFolderName(display); err != nil {
		return Folder{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.loadFolder(name)
	if err != nil {
		return Folder{}, err
	}
	if f.State != StateActive {
		return Folder{}, apierror.FailedPrecondition("folder %s is pending deletion; undelete it before updating it", f.Name)
	}
	all, err := r.folders()
	if err != nil {
		return Folder{}, err
	}
	if err := r.checkSiblingName(all, f.Parent, display, f.Name); err != nil {
		return Folder{}, err
	}
	f.DisplayName = display
	f.UpdateTime = r.now().UTC().Format(time.RFC3339)
	return f, r.putFolder(f)
}

// MoveFolder places a folder under another parent.
func (r *Registry) MoveFolder(name, parent string) (Folder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.loadFolder(name)
	if err != nil {
		return Folder{}, err
	}
	if f.State != StateActive {
		return Folder{}, apierror.FailedPrecondition("folder %s is pending deletion; undelete it before moving it", f.Name)
	}
	depth, err := r.checkParentLocked(parent)
	if err != nil {
		return Folder{}, err
	}
	// The destination must not be the folder or one of its descendants.
	for p := parent; strings.HasPrefix(p, "folders/"); {
		if p == f.Name {
			return Folder{}, apierror.InvalidArgument("folder %s cannot be moved under itself or one of its descendants", f.Name)
		}
		pf, err := r.loadFolder(p)
		if err != nil {
			return Folder{}, err
		}
		p = pf.Parent
	}
	all, err := r.folders()
	if err != nil {
		return Folder{}, err
	}
	if depth+subtreeDepth(all, f.Name) > MaxFolderDepth {
		return Folder{}, apierror.FailedPrecondition("the move would nest folders more than %d deep", MaxFolderDepth)
	}
	if err := r.checkSiblingName(all, parent, f.DisplayName, f.Name); err != nil {
		return Folder{}, err
	}
	f.Parent = parent
	f.UpdateTime = r.now().UTC().Format(time.RFC3339)
	return f, r.putFolder(f)
}

// DeleteFolder marks an empty folder DELETE_REQUESTED. One with an active
// folder or project under it is FAILED_PRECONDITION, as on Google.
func (r *Registry) DeleteFolder(name string) (Folder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.loadFolder(name)
	if err != nil {
		return Folder{}, err
	}
	if f.State != StateActive {
		return Folder{}, apierror.FailedPrecondition("folder %s is already pending deletion", f.Name)
	}
	all, err := r.folders()
	if err != nil {
		return Folder{}, err
	}
	for _, c := range all {
		if c.Parent == f.Name && c.State == StateActive {
			return Folder{}, apierror.FailedPrecondition("folder %s is not empty: it contains %s", f.Name, c.Name)
		}
	}
	projects, err := r.st.List(keyPrefix)
	if err != nil {
		return Folder{}, apierror.Internal(err, "list projects")
	}
	for _, k := range projects {
		p, err := r.load(strings.TrimPrefix(k, keyPrefix))
		if err == nil && p.Parent == f.Name && p.State == StateActive {
			return Folder{}, apierror.FailedPrecondition("folder %s is not empty: it contains project %s", f.Name, p.ProjectID)
		}
	}
	now := r.now().UTC().Format(time.RFC3339)
	f.State, f.DeleteTime, f.UpdateTime = StateDeleteRequested, now, now
	return f, r.putFolder(f)
}

// UndeleteFolder restores a folder pending deletion. Its parent must be
// active.
func (r *Registry) UndeleteFolder(name string) (Folder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.loadFolder(name)
	if err != nil {
		return Folder{}, err
	}
	if f.State != StateDeleteRequested {
		return Folder{}, apierror.FailedPrecondition("folder %s is not pending deletion; only a DELETE_REQUESTED folder can be undeleted", f.Name)
	}
	if _, err := r.checkParentLocked(f.Parent); err != nil {
		return Folder{}, apierror.FailedPrecondition("folder %s cannot be undeleted: its parent: %v", f.Name, err)
	}
	all, err := r.folders()
	if err != nil {
		return Folder{}, err
	}
	if err := r.checkSiblingName(all, f.Parent, f.DisplayName, f.Name); err != nil {
		return Folder{}, err
	}
	f.State, f.DeleteTime = StateActive, ""
	f.UpdateTime = r.now().UTC().Format(time.RFC3339)
	return f, r.putFolder(f)
}

// Ancestor is one step of a resource's ancestry.
type Ancestor struct {
	// Type is "project", "folder" or "organization".
	Type string
	// ID is the project ID, or the folder's or organization's number.
	ID string
}

// Ancestry is a project's ancestry, the project first and the organization,
// when it has one, last.
func (r *Registry) Ancestry(projectID string) ([]Ancestor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.load(projectID)
	if err != nil {
		return nil, err
	}
	out := []Ancestor{{Type: "project", ID: p.ProjectID}}
	for parent, i := p.Parent, 0; parent != "" && i <= MaxFolderDepth+1; i++ {
		if parent == OrganizationName {
			out = append(out, Ancestor{Type: "organization", ID: OrganizationNumber})
			break
		}
		f, err := r.loadFolder(parent)
		if err != nil {
			return nil, err
		}
		out = append(out, Ancestor{Type: "folder", ID: f.ID()})
		parent = f.Parent
	}
	return out, nil
}
