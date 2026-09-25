// Package resource parses and formats Google resource names and applies
// project and location scoping.
//
// It is the only place that understands name syntax, so the collision and
// traversal tests have a single surface to cover (docs/architecture.md §3).
//
// Names follow the google.api.resource patterns in the pinned protos. Parsing
// is strict: a name that merely looks plausible is rejected, because accepting
// it would let a caller address a resource CloudBurrow never meant to expose.
package resource

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrMalformed means a name does not match its pattern.
var ErrMalformed = errors.New("malformed resource name")

// Name is a parsed resource name.
type Name struct {
	Project  string
	Location string
	// Collection is the resource collection, e.g. "queues" or "services".
	Collection string
	// ID is the resource ID within the collection.
	ID string
	// Parent is the nested parent ID, for names like queues/{q}/tasks/{t}.
	ParentCollection string
	ParentID         string
}

// Google resource ID rules differ per service; these are the shared shapes.
var (
	// projectRE matches a project ID: 6-30 chars, lowercase letter first.
	projectRE = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	// locationRE matches a location such as us-central1.
	locationRE = regexp.MustCompile(`^[a-z]+(-[a-z0-9]+)*$`)
	// idRE matches a resource ID. Deliberately excludes "/" and "..": a name
	// is untrusted input and must never be able to address a parent path.
	idRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._~%+-]{0,499}$`)
)

// ValidID reports whether an ID is usable as a resource identifier.
//
// The check is explicit about traversal because resource IDs reach the
// filesystem and the Kubernetes API: "..", an encoded separator or a leading
// slash must never survive parsing.
func ValidID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	if strings.ContainsAny(id, "/\\") {
		return false
	}
	// Reject encoded separators and traversal in any case form.
	lower := strings.ToLower(id)
	for _, bad := range []string{"%2f", "%5c", "%2e%2e", "..", "\x00"} {
		if strings.Contains(lower, bad) {
			return false
		}
	}
	return idRE.MatchString(id)
}

// ValidProjectID reports whether id is a Google project ID: 6-30 characters,
// lowercase letters, digits and hyphens, starting with a letter and not ending
// with a hyphen.
//
// Exported so the one rule that every resource name is checked against is also
// the rule anything choosing a project checks against. The instance's default
// project used to be taken from its name without this check, so a valid instance
// name could produce a project every service then refused.
func ValidProjectID(id string) bool { return projectRE.MatchString(id) }

// ParseProject parses "projects/{project}".
func ParseProject(name string) (string, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 2 || parts[0] != "projects" {
		return "", fmt.Errorf("%w: %q is not projects/{project}", ErrMalformed, name)
	}
	if !projectRE.MatchString(parts[1]) {
		return "", fmt.Errorf("%w: project %q is not a valid project ID", ErrMalformed, parts[1])
	}
	return parts[1], nil
}

// ParseLocation parses "projects/{project}/locations/{location}".
func ParseLocation(name string) (project, location string, err error) {
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "locations" {
		return "", "", fmt.Errorf("%w: %q is not projects/{project}/locations/{location}", ErrMalformed, name)
	}
	if !projectRE.MatchString(parts[1]) {
		return "", "", fmt.Errorf("%w: project %q is not valid", ErrMalformed, parts[1])
	}
	if !locationRE.MatchString(parts[3]) {
		return "", "", fmt.Errorf("%w: location %q is not valid", ErrMalformed, parts[3])
	}
	return parts[1], parts[3], nil
}

// Parse parses a location-scoped resource name, with an optional nested child:
//
//	projects/{p}/locations/{l}/{collection}/{id}
//	projects/{p}/locations/{l}/{collection}/{id}/{child}/{childID}
func Parse(name string) (Name, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 6 && len(parts) != 8 {
		return Name{}, fmt.Errorf("%w: %q has %d segments, want 6 or 8", ErrMalformed, name, len(parts))
	}
	project, location, err := ParseLocation(strings.Join(parts[:4], "/"))
	if err != nil {
		return Name{}, err
	}
	n := Name{Project: project, Location: location}

	if parts[4] == "" || !ValidID(parts[5]) {
		return Name{}, fmt.Errorf("%w: %q contains an invalid resource ID", ErrMalformed, name)
	}
	n.Collection, n.ID = parts[4], parts[5]

	if len(parts) == 8 {
		if parts[6] == "" || !ValidID(parts[7]) {
			return Name{}, fmt.Errorf("%w: %q contains an invalid child ID", ErrMalformed, name)
		}
		n.ParentCollection, n.ParentID = n.Collection, n.ID
		n.Collection, n.ID = parts[6], parts[7]
	}
	return n, nil
}

// String formats the name back to its canonical form.
func (n Name) String() string {
	base := fmt.Sprintf("projects/%s/locations/%s", n.Project, n.Location)
	if n.ParentCollection != "" {
		return fmt.Sprintf("%s/%s/%s/%s/%s", base, n.ParentCollection, n.ParentID, n.Collection, n.ID)
	}
	return fmt.Sprintf("%s/%s/%s", base, n.Collection, n.ID)
}

// LocationName returns the location-scoped parent of this resource.
func (n Name) LocationName() string {
	return fmt.Sprintf("projects/%s/locations/%s", n.Project, n.Location)
}

// Key returns a storage key that keeps projects and locations isolated.
//
// Isolation is structural rather than conventional: two resources with the
// same ID in different projects produce different keys by construction, so a
// collision cannot be introduced by a caller forgetting to prefix.
func (n Name) Key() string { return n.String() }

// ProjectNumber is a stable number for a project ID. The registry keeps
// none, so it is derived: the same every time for the same ID, the same in
// every service that reports one, and never claimed to match a real
// project's.
func ProjectNumber(id string) int64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(id); i++ {
		h ^= uint64(id[i])
		h *= 1099511628211
	}
	return int64(h%900000000000) + 100000000000
}
