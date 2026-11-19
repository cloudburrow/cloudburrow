package run

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// A build's service account (#1854), as Google's "Configure user-specified
// service accounts" page documents it.
//
// Build.serviceAccount and BuildTrigger.serviceAccount are
// projects/PROJECT_ID/serviceAccounts/ACCOUNT, ACCOUNT the account's email
// or unique ID. A triggered build runs as its trigger's account; the build
// config's own serviceAccount is ignored then, as Google's note says. A
// build with no account runs as the project's default Cloud Build account,
// the one GetDefaultServiceAccount names.
//
// A build run as a user-specified account cannot write its log to Google's
// default logs bucket: it must name logsBucket, or log to Cloud Logging
// only (CLOUD_LOGGING_ONLY) or not at all (NONE). Google refuses it
// INVALID_ARGUMENT with the message buildSALoggingError quotes.
//
// Every arbitrary step reaches the metadata server under its account's
// host (metadataaccount.go): GCE_METADATA_HOST names it, so the email the
// metadata server gives, and the account its access and identity tokens
// are for, are the build's.

// buildSALoggingError is Google's refusal of a user-specified account
// whose log would go to the Google-owned default bucket.
const buildSALoggingError = "if 'build.service_account' is specified, the build must either (a) specify 'build.logs_bucket', " +
	"(b) use the REGIONAL_USER_OWNED_BUCKET build.options.default_logs_bucket_behavior option, " +
	"or (c) use either CLOUD_LOGGING_ONLY / NONE logging options"

var buildSARE = regexp.MustCompile(`^projects/([^/]+)/serviceAccounts/([^/]+)$`)

// ServiceAccountLookup resolves projects/P/serviceAccounts/{email or unique
// ID} through the instance's IAM: the account's email and whether it is
// disabled, or found false for no such account.
type ServiceAccountLookup func(name string) (email string, disabled, found bool)

var serviceAccounts struct {
	mu     sync.Mutex
	lookup ServiceAccountLookup
}

// UseServiceAccounts sets the IAM lookup a build's serviceAccount is
// checked against. Without one, an email is taken as named and a unique ID
// is refused.
func UseServiceAccounts(l ServiceAccountLookup) {
	serviceAccounts.mu.Lock()
	serviceAccounts.lookup = l
	serviceAccounts.mu.Unlock()
}

// DefaultBuildAccount is the account a build with no serviceAccount runs
// as: the project's Cloud Build service account.
func DefaultBuildAccount(project string) string {
	return fmt.Sprintf("%d@cloudbuild.gserviceaccount.com", resource.ProjectNumber(project))
}

// checkServiceAccountName checks a serviceAccount field's form.
func checkServiceAccountName(field, name string) error {
	if !buildSARE.MatchString(name) {
		return apierror.InvalidArgument("invalid value for '%s': %q must be of the form "+
			"projects/{PROJECT_ID}/serviceAccounts/{ACCOUNT}, ACCOUNT the account's email or unique ID", field, name)
	}
	return nil
}

// buildAccount is the email of the account a build runs as, refusing a
// serviceAccount that is malformed, names no account, or whose log would
// go to Google's default bucket.
func buildAccount(project string, b *cbpb.Build) (string, error) {
	name := b.GetServiceAccount()
	if name == "" {
		return DefaultBuildAccount(project), nil
	}
	if err := checkServiceAccountName("build.service_account", name); err != nil {
		return "", err
	}
	logging := b.GetOptions().GetLogging()
	if b.GetLogsBucket() == "" && logging != cbpb.BuildOptions_CLOUD_LOGGING_ONLY && logging != cbpb.BuildOptions_NONE {
		return "", apierror.InvalidArgument(buildSALoggingError)
	}
	acct := buildSARE.FindStringSubmatch(name)[2]
	serviceAccounts.mu.Lock()
	lookup := serviceAccounts.lookup
	serviceAccounts.mu.Unlock()
	// A user-managed account lives in IAM; Google's own accounts
	// (…@cloudbuild.gserviceaccount.com, …-compute@developer…) do not.
	userManaged := !strings.Contains(acct, "@") || strings.HasSuffix(acct, ".iam.gserviceaccount.com")
	if !userManaged {
		return acct, nil
	}
	if lookup == nil {
		if !strings.Contains(acct, "@") {
			return "", apierror.Unimplemented("build.service_account by unique ID needs the instance's IAM, which is not served here; name the account by email")
		}
		return acct, nil
	}
	email, disabled, found := lookup(name)
	switch {
	case !found:
		return "", apierror.InvalidArgument("invalid value for 'build.service_account': service account %s does not exist", name)
	case disabled:
		return "", apierror.FailedPrecondition("invalid value for 'build.service_account': service account %s is disabled", name)
	}
	return email, nil
}
