// Package wif is Workload Identity Federation's shared logic (#1941): the
// attribute mapping and attribute condition in CEL, the verification of an
// external OIDC token against a provider's JWKS, and the principal
// identifiers a federated token's subject is known by in IAM policies.
//
// The IAM API's pools and providers (internal/service/resourcemanager's
// workloadidentity.go) validate with it; the Security Token Service
// (internal/metadata's sts.go) exchanges with it. Behaviour follows Google's
// pages (docs.cloud.google.com/iam/docs/workload-identity-federation,
// .../iam/docs/principal-identifiers, the IAM v1
// projects.locations.workloadIdentityPools.providers reference and the STS
// v1 token reference, read 2026-11-19).
package wif

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/ext"
)

// Limits Google documents for attribute mappings and conditions.
const (
	MaxMappingExpression   = 2048
	MaxConditionExpression = 4096
	MaxCustomAttributes    = 50
	MaxAttributeKey        = 100
	MaxSubjectBytes        = 127
	MaxMappedBytes         = 8 * 1024
)

var customKey = regexp.MustCompile(`^[a-z0-9_]+$`)

// Attributes are what a mapping produced from one assertion.
type Attributes struct {
	Subject     string
	Groups      []string
	DisplayName string
	// Custom are attribute.NAME's values; a list attribute has several.
	Custom map[string][]string
	// Google is every google.* value as mapped, for the condition.
	Google map[string]any
}

var (
	envOnce sync.Once
	env     *cel.Env
	envErr  error
)

// Env is the CEL environment mappings and conditions compile in:
// `assertion`, `google` and `attribute`, Google's extract() and the
// strings extension.
func Env() (*cel.Env, error) {
	envOnce.Do(func() {
		env, envErr = cel.NewEnv(
			cel.Variable("assertion", cel.DynType),
			cel.Variable("google", cel.MapType(cel.StringType, cel.DynType)),
			cel.Variable("attribute", cel.MapType(cel.StringType, cel.DynType)),
			ext.Strings(),
			cel.Function("extract",
				cel.MemberOverload("string_extract_string", []*cel.Type{cel.StringType, cel.StringType}, cel.StringType,
					cel.BinaryBinding(func(s, t ref.Val) ref.Val {
						return types.String(Extract(string(s.(types.String)), string(t.(types.String))))
					}))),
		)
	})
	return env, envErr
}

// Extract is Google's extract(): template is "before{name}after", and the
// result is the text between the first `before` and the next `after`, ""
// when either is missing (an empty `before` starts at the beginning, an
// empty `after` runs to the end).
func Extract(s, template string) string {
	open := strings.Index(template, "{")
	end := strings.Index(template, "}")
	if open < 0 || end < open {
		return ""
	}
	before, after := template[:open], template[end+1:]
	i := 0
	if before != "" {
		j := strings.Index(s, before)
		if j < 0 {
			return ""
		}
		i = j + len(before)
	}
	rest := s[i:]
	if after == "" {
		return rest
	}
	k := strings.Index(rest, after)
	if k < 0 {
		return ""
	}
	return rest[:k]
}

func compile(src string) (cel.Program, error) {
	e, err := Env()
	if err != nil {
		return nil, err
	}
	ast, iss := e.Compile(src)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	return e.Program(ast)
}

// ValidateMapping checks an attribute mapping as Google does on create and
// update; the error's text is the INVALID_ARGUMENT message.
func ValidateMapping(m map[string]string, requireSubject bool) error {
	return validateMapping(m, requireSubject, false)
}

// ValidateWorkforceMapping checks a workforce pool provider's attribute
// mapping (#2056): google.subject is required, and google.profile_photo
// is a key too.
func ValidateWorkforceMapping(m map[string]string) error {
	if len(m) == 0 {
		return fmt.Errorf("The attribute mapping is required and must include a mapping to google.subject")
	}
	return validateMapping(m, true, true)
}

func validateMapping(m map[string]string, requireSubject, workforce bool) error {
	custom := 0
	for k, v := range m {
		switch {
		case k == "google.subject", k == "google.groups", k == "google.display_name", k == "google.posix_username",
			workforce && k == "google.profile_photo":
		case strings.HasPrefix(k, "attribute."):
			name := strings.TrimPrefix(k, "attribute.")
			if name == "" || len(name) > MaxAttributeKey || !customKey.MatchString(name) {
				return fmt.Errorf("Invalid attribute mapping key %q: a custom attribute name is 1 to %d characters of [a-z0-9_]", k, MaxAttributeKey)
			}
			custom++
		default:
			return fmt.Errorf("Invalid attribute mapping key %q: the supported keys are google.subject, google.groups, google.display_name, google.posix_username and attribute.{custom_attribute}", k)
		}
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("The attribute mapping for %q is empty", k)
		}
		if len(v) > MaxMappingExpression {
			return fmt.Errorf("The attribute mapping expression for %q exceeds %d characters", k, MaxMappingExpression)
		}
		if _, err := compile(v); err != nil {
			return fmt.Errorf("Invalid attribute mapping expression for %q: %v", k, err)
		}
	}
	if custom > MaxCustomAttributes {
		return fmt.Errorf("At most %d custom attributes may be mapped, got %d", MaxCustomAttributes, custom)
	}
	if _, ok := m["google.subject"]; !ok && (requireSubject || len(m) > 0) {
		return fmt.Errorf("The attribute mapping must include a mapping to google.subject")
	}
	return nil
}

// ValidateCondition checks an attribute condition.
func ValidateCondition(src string) error {
	if src == "" {
		return nil
	}
	if len(src) > MaxConditionExpression {
		return fmt.Errorf("The attribute condition exceeds %d characters", MaxConditionExpression)
	}
	e, err := Env()
	if err != nil {
		return err
	}
	ast, iss := e.Compile(src)
	if iss.Err() != nil {
		return fmt.Errorf("Invalid attribute condition: %v", iss.Err())
	}
	if t := ast.OutputType(); !t.IsExactType(cel.BoolType) && !t.IsExactType(cel.DynType) {
		return fmt.Errorf("The attribute condition must evaluate to a bool, not %s", t)
	}
	return nil
}

// ErrSubjectDeleted is an exchange for a workforce pool subject deleted
// less than 30 days ago (#2056).
var ErrSubjectDeleted = fmt.Errorf("The workforce pool subject is deleted: its google.subject cannot exchange tokens until it is undeleted, or 30 days after it was deleted.")

// ErrCondition is an assertion the attribute condition refuses.
var ErrCondition = fmt.Errorf("The given credential is rejected by the attribute condition.")

// Apply evaluates mapping over assertion, then condition over the result.
func Apply(mapping map[string]string, condition string, assertion map[string]any) (Attributes, error) {
	return apply(mapping, condition, assertion, MaxMappedBytes, 0)
}

// Workforce pool limits (#2056): the mapped attributes total at most 16
// KB, and google.display_name at most 100 bytes.
const (
	MaxWorkforceMappedBytes = 16 * 1024
	MaxDisplayNameBytes     = 100
)

// ApplyWorkforce is Apply with a workforce pool provider's limits.
func ApplyWorkforce(mapping map[string]string, condition string, assertion map[string]any) (Attributes, error) {
	return apply(mapping, condition, assertion, MaxWorkforceMappedBytes, MaxDisplayNameBytes)
}

func apply(mapping map[string]string, condition string, assertion map[string]any, maxMapped, maxDisplay int) (Attributes, error) {
	out := Attributes{Custom: map[string][]string{}, Google: map[string]any{}}
	attr := map[string]any{}
	keys := make([]string, 0, len(mapping))
	for k := range mapping {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	size := 0
	for _, k := range keys {
		prg, err := compile(mapping[k])
		if err != nil {
			return out, fmt.Errorf("The attribute mapping for %s is invalid: %v", k, err)
		}
		v, _, err := prg.Eval(map[string]any{"assertion": assertion, "google": map[string]any{}, "attribute": map[string]any{}})
		if err != nil {
			// A claim the assertion lacks maps to nothing, as Google's
			// mapping of a missing field does.
			continue
		}
		val := v.Value()
		strs := stringsOf(val)
		for _, s := range strs {
			size += len(s)
		}
		switch {
		case k == "google.subject":
			if len(strs) != 1 {
				return out, fmt.Errorf("The mapped attribute google.subject must be a string")
			}
			out.Subject = strs[0]
			out.Google["subject"] = strs[0]
		case k == "google.groups":
			out.Groups = strs
			out.Google["groups"] = strs
		case strings.HasPrefix(k, "google."):
			s := strings.Join(strs, ",")
			if k == "google.display_name" {
				out.DisplayName = s
			}
			out.Google[strings.TrimPrefix(k, "google.")] = s
		default:
			name := strings.TrimPrefix(k, "attribute.")
			out.Custom[name] = strs
			if len(strs) == 1 {
				attr[name] = strs[0]
			} else {
				attr[name] = strs
			}
		}
	}
	if out.Subject == "" {
		return out, fmt.Errorf("The mapped attribute google.subject must not be empty")
	}
	if len(out.Subject) > MaxSubjectBytes {
		return out, fmt.Errorf("The mapped attribute google.subject exceeds %d bytes", MaxSubjectBytes)
	}
	if size > maxMapped {
		return out, fmt.Errorf("The total size of the mapped attributes exceeds %d bytes", maxMapped)
	}
	if maxDisplay > 0 && len(out.DisplayName) > maxDisplay {
		return out, fmt.Errorf("The mapped attribute google.display_name exceeds %d bytes", maxDisplay)
	}
	if condition != "" {
		prg, err := compile(condition)
		if err != nil {
			return out, ErrCondition
		}
		g := map[string]any{}
		for k, v := range out.Google {
			g[k] = v
		}
		v, _, err := prg.Eval(map[string]any{"assertion": assertion, "google": g, "attribute": attr})
		if err != nil || v.Value() != true {
			return out, ErrCondition
		}
	}
	return out, nil
}

// stringsOf is a mapped value as strings: a string, or a list of them;
// any other scalar in its text form.
func stringsOf(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
		return out
	case []ref.Val:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, fmt.Sprint(e.Value()))
		}
		return out
	case []string:
		return x
	case ref.Val:
		return stringsOf(x.Value())
	}
	return []string{fmt.Sprint(v)}
}

// PoolName is a pool's resource name under its project number.
func PoolName(projectNumber, pool string) string {
	return "projects/" + projectNumber + "/locations/global/workloadIdentityPools/" + pool
}

// Principal is the subject's principal:// identifier.
func Principal(projectNumber, pool, subject string) string {
	return "principal://iam.googleapis.com/" + PoolName(projectNumber, pool) + "/subject/" + subject
}

// Sets are the principalSet:// identifiers that name a federated identity
// with attrs: every identity in the pool, each of its groups and each
// value of each custom attribute.
func Sets(projectNumber, pool string, attrs Attributes) []string {
	base := "principalSet://iam.googleapis.com/" + PoolName(projectNumber, pool)
	out := []string{base + "/*"}
	for _, g := range attrs.Groups {
		out = append(out, base+"/group/"+g)
	}
	names := make([]string, 0, len(attrs.Custom))
	for n := range attrs.Custom {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, v := range attrs.Custom[n] {
			out = append(out, base+"/attribute."+n+"/"+v)
		}
	}
	return out
}

// PoolOfPrincipal is the "projects/N/locations/global/workloadIdentityPools/P"
// a principal:// or principalSet:// identifier names; ok is false for any
// other member.
func PoolOfPrincipal(member string) (projectNumber, pool string, ok bool) {
	rest, found := strings.CutPrefix(member, "principal://iam.googleapis.com/projects/")
	if !found {
		rest, found = strings.CutPrefix(member, "principalSet://iam.googleapis.com/projects/")
	}
	if !found {
		lower := strings.ToLower(member)
		for _, p := range []string{"principal://iam.googleapis.com/projects/", "principalset://iam.googleapis.com/projects/"} {
			if strings.HasPrefix(lower, p) {
				rest, found = member[len(p):], true
			}
		}
	}
	if !found {
		return "", "", false
	}
	parts := strings.SplitN(rest, "/", 6)
	if len(parts) < 5 || parts[1] != "locations" || parts[2] != "global" || !strings.EqualFold(parts[3], "workloadIdentityPools") {
		return "", "", false
	}
	return parts[0], parts[4], true
}

// WorkforcePoolName is a workforce pool's resource name (#2056): pools
// are global, under an organization that the name does not show.
func WorkforcePoolName(pool string) string {
	return "locations/global/workforcePools/" + pool
}

// WorkforcePrincipal is a workforce pool subject's principal://
// identifier.
func WorkforcePrincipal(pool, subject string) string {
	return "principal://iam.googleapis.com/" + WorkforcePoolName(pool) + "/subject/" + subject
}

// WorkforceSets are the principalSet:// identifiers that name a workforce
// identity with attrs: every identity in the pool, each of its groups and
// each value of each custom attribute.
func WorkforceSets(pool string, attrs Attributes) []string {
	base := "principalSet://iam.googleapis.com/" + WorkforcePoolName(pool)
	out := []string{base + "/*"}
	for _, g := range attrs.Groups {
		out = append(out, base+"/group/"+g)
	}
	names := make([]string, 0, len(attrs.Custom))
	for n := range attrs.Custom {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, v := range attrs.Custom[n] {
			out = append(out, base+"/attribute."+n+"/"+v)
		}
	}
	return out
}

// WorkforcePoolOfPrincipal is the workforce pool a
// principal://iam.googleapis.com/locations/global/workforcePools/P/... or
// principalSet:// identifier names, and for a principal:// its subject;
// ok is false for any other member.
func WorkforcePoolOfPrincipal(member string) (pool, subject string, ok bool) {
	lower := strings.ToLower(member)
	rest := ""
	single := false
	for _, p := range []string{"principal://iam.googleapis.com/locations/global/workforcepools/", "principalset://iam.googleapis.com/locations/global/workforcepools/"} {
		if strings.HasPrefix(lower, p) {
			rest, single, ok = member[len(p):], strings.HasPrefix(p, "principal:"), true
		}
	}
	if !ok {
		return "", "", false
	}
	pool, tail, found := strings.Cut(rest, "/")
	if pool == "" || !found || tail == "" {
		return "", "", false
	}
	if single {
		sub, isSubject := strings.CutPrefix(tail, "subject/")
		if !isSubject || sub == "" {
			return "", "", false
		}
		return pool, sub, true
	}
	return pool, "", true
}

// IsFederated reports whether member is a workload or workforce identity
// pool's principal:// or principalSet:// identifier.
func IsFederated(member string) bool {
	if _, _, ok := PoolOfPrincipal(member); ok {
		return true
	}
	_, _, ok := WorkforcePoolOfPrincipal(member)
	return ok
}

// Provider kinds.
const (
	KindOIDC = "oidc"
	KindAWS  = "aws"
	KindSAML = "saml"
	KindX509 = "x509"
)

// Provider is a live provider, as the Security Token Service needs it.
type Provider struct {
	ProjectNumber    string
	Pool             string
	ID               string
	Kind             string
	IssuerURI        string
	AllowedAudiences []string
	JWKSJSON         string
	DiscoveryURI     string
	Mapping          map[string]string
	Condition        string
	// Workforce is a workforce pool's provider (#2056): ProjectNumber is
	// empty, the audience is ClientID, the token lasts SessionTTL and
	// ProgrammaticDisabled refuses every exchange.
	Workforce            bool
	ClientID             string
	SessionTTL           time.Duration
	ProgrammaticDisabled bool
	// SAMLMetadata is a SAML provider's IdP metadata document.
	SAMLMetadata string
}

// Name is the provider's canonical resource name, //iam.googleapis.com/....
func (p Provider) Name() string {
	if p.Workforce {
		return "//iam.googleapis.com/" + WorkforcePoolName(p.Pool) + "/providers/" + p.ID
	}
	return "//iam.googleapis.com/" + PoolName(p.ProjectNumber, p.Pool) + "/providers/" + p.ID
}
