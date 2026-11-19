package secrets

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Version aliases (#1850): Secret.version_aliases maps an alias to a version
// number. The rules are the field's own documentation in
// google/cloud/secretmanager/v1/resources.proto and
// https://docs.cloud.google.com/secret-manager/docs/assign-alias-to-secret-version:
// at most 63 characters of letters, numerals, hyphens and underscores,
// starting with a letter; never "latest" or "NEW" (case-insensitively, per
// the docs page); no more than 50 on a secret. Aliases are set with
// UpdateSecret and read with GetSecret, and a version is reached by its alias
// only through GetSecretVersion and AccessSecretVersion.

// MaxVersionAliases is the documented limit of aliases on one secret.
const MaxVersionAliases = 50

var aliasPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,62}$`)

// ValidateVersionAlias checks one alias name against the documented rules.
func ValidateVersionAlias(alias string) error {
	if !aliasPattern.MatchString(alias) {
		return apierror.InvalidArgument("version alias %q must start with a letter and hold at most 63 letters, "+
			"numerals, hyphens and underscores", alias)
	}
	if l := strings.ToLower(alias); l == "latest" || l == "new" {
		return apierror.InvalidArgument("version alias %q is reserved; latest and NEW cannot be aliases", alias)
	}
	return nil
}

// checkVersionAliases validates a whole alias map for a secret whose
// versions are those numbered below next. A version that was never added
// is refused: the alias would resolve to nothing. UNVERIFIED: the error
// Google returns for an alias to a version that does not exist is not
// documented; INVALID_ARGUMENT is CloudBurrow's choice.
func checkVersionAliases(aliases map[string]int, exists func(int) bool) error {
	if len(aliases) > MaxVersionAliases {
		return apierror.InvalidArgument("a secret can have no more than %d version aliases, got %d",
			MaxVersionAliases, len(aliases))
	}
	names := make([]string, 0, len(aliases))
	for a := range aliases {
		names = append(names, a)
	}
	sort.Strings(names)
	for _, a := range names {
		if err := ValidateVersionAlias(a); err != nil {
			return err
		}
		n := aliases[a]
		if n < 1 {
			return apierror.InvalidArgument("version alias %q must name a positive version number, got %d", a, n)
		}
		if !exists(n) {
			return apierror.InvalidArgument("version alias %q names version %d, which does not exist", a, n)
		}
	}
	return nil
}

// isVersionNumber reports whether a version reference is a number.
func isVersionNumber(version string) bool {
	_, err := strconv.Atoi(version)
	return err == nil
}

// SortedAliases is a secret's aliases in name order, as "alias=N" pairs'
// parts, for a stable page.
func SortedAliases(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for a := range m {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
