package privateca

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"sync"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cloud.google.com/go/security/privateca/apiv1/privatecapb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// identity_constraints.cel_expression, in the dialect Google documents for
// issuance policies and certificate templates
// (https://docs.cloud.google.com/certificate-authority-service/docs/using-cel):
// `subject` has the string fields common_name, country_code, organization,
// organizational_unit, locality, province, street_address and postal_code;
// `subject_alt_names` is a list whose items have value (string), oid
// (list of int) and type, one of DNS, URI, EMAIL, IP_ADDRESS and CUSTOM;
// "CA Service doesn't support CEL expressions with nested macros". The
// expression validates "the resolved X.509 Subject and/or Subject
// Alternative Name before a certificate is signed".

// SAN types, as the dialect's enum constants.
const (
	sanDNS = iota + 1
	sanURI
	sanEmail
	sanIP
	sanCustom
)

var (
	celEnvOnce sync.Once
	celEnv     *cel.Env
	celEnvErr  error
)

func identityEnv() (*cel.Env, error) {
	celEnvOnce.Do(func() {
		celEnv, celEnvErr = cel.NewEnv(
			cel.Variable("subject", cel.MapType(cel.StringType, cel.StringType)),
			cel.Variable("subject_alt_names", cel.ListType(cel.MapType(cel.StringType, cel.DynType))),
			cel.Constant("DNS", cel.IntType, types.Int(sanDNS)),
			cel.Constant("URI", cel.IntType, types.Int(sanURI)),
			cel.Constant("EMAIL", cel.IntType, types.Int(sanEmail)),
			cel.Constant("IP_ADDRESS", cel.IntType, types.Int(sanIP)),
			cel.Constant("CUSTOM", cel.IntType, types.Int(sanCustom)),
			cel.ASTValidators(cel.ValidateComprehensionNestingLimit(1)),
		)
	})
	return celEnv, celEnvErr
}

// compileIdentity compiles a cel_expression to a boolean program; field
// names where it is set ("issuance_policy.identity_constraints" or
// "identity_constraints").
func compileIdentity(field string, ic *privatecapb.CertificateIdentityConstraints) (cel.Program, error) {
	src := ic.GetCelExpression().GetExpression()
	if src == "" {
		return nil, nil
	}
	env, err := identityEnv()
	if err != nil {
		return nil, apierror.Internal(err, "the CEL environment")
	}
	ast, iss := env.Compile(src)
	if iss.Err() != nil {
		return nil, apierror.InvalidArgument("%s.cel_expression is not a valid expression: %v", field, iss.Err())
	}
	if !ast.OutputType().IsExactType(cel.BoolType) && !ast.OutputType().IsExactType(cel.DynType) {
		return nil, apierror.InvalidArgument("%s.cel_expression must evaluate to a bool, not %s", field, ast.OutputType())
	}
	prg, err := env.Program(ast)
	if err != nil {
		return nil, apierror.InvalidArgument("%s.cel_expression: %v", field, err)
	}
	return prg, nil
}

// checkIdentity evaluates a cel_expression against a certificate's
// resolved subject and SANs; false or an evaluation error refuses it.
func checkIdentity(field string, ic *privatecapb.CertificateIdentityConstraints, t *x509.Certificate) error {
	prg, err := compileIdentity(field, ic)
	if err != nil || prg == nil {
		return err
	}
	first := func(v []string) string {
		if len(v) == 0 {
			return ""
		}
		return v[0]
	}
	n := t.Subject
	if len(t.RawSubject) > 0 {
		var seq pkix.RDNSequence
		if _, err := asn1Unmarshal(t.RawSubject, &seq); err == nil {
			n = pkix.Name{}
			n.FillFromRDNSequence(&seq)
		}
	}
	subject := map[string]string{
		"common_name": n.CommonName, "country_code": first(n.Country), "organization": first(n.Organization),
		"organizational_unit": first(n.OrganizationalUnit), "locality": first(n.Locality), "province": first(n.Province),
		"street_address": first(n.StreetAddress), "postal_code": first(n.PostalCode),
	}
	var sans []map[string]any
	add := func(typ int, v string) {
		sans = append(sans, map[string]any{"type": typ, "value": v, "oid": []int{}})
	}
	for _, d := range t.DNSNames {
		add(sanDNS, d)
	}
	for _, u := range t.URIs {
		add(sanURI, u.String())
	}
	for _, e := range t.EmailAddresses {
		add(sanEmail, e)
	}
	for _, ip := range t.IPAddresses {
		add(sanIP, ip.String())
	}
	out, _, err := prg.Eval(map[string]any{"subject": subject, "subject_alt_names": sans})
	if err != nil {
		return apierror.InvalidArgument("the certificate request does not satisfy %s.cel_expression %q: %v", field, ic.GetCelExpression().GetExpression(), err)
	}
	if ok, isBool := out.Value().(bool); !isBool || !ok {
		return apierror.InvalidArgument("the certificate request does not satisfy %s.cel_expression %q", field, ic.GetCelExpression().GetExpression())
	}
	return nil
}
