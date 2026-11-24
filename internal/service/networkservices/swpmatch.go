package networkservices

import "github.com/cloudburrow/cloudburrow/internal/swpmatch"

// CheckMatcher compiles a sessionMatcher (application false) or an
// applicationMatcher (internal/swpmatch).
func CheckMatcher(expr string, application bool) error {
	return swpmatch.CheckMatcher(expr, application)
}

// ValidURLListEntry is whether a URL list entry is valid
// (internal/swpmatch).
func ValidURLListEntry(e string) bool { return swpmatch.ValidURLListEntry(e) }

func urlListMatches(value string, entries []string) bool {
	return swpmatch.URLListMatches(value, entries)
}
