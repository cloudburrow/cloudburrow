package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/security/privateca/apiv1/privatecapb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Certificate Authority Service console parity, 2026-11-22 round (#2144).
// From Google's public docs (Console tabs of creating-ca-pool,
// creating-root-ca, create-subordinate-ca, managing-ca-state,
// deleting-certificate-authorities, requesting-certificates,
// revoking-certificates, use-issuance-policy, creating-certificate-template
// and view-issued-certificates, read 2026-11-22):
//
//   - the Certificate Authority Service page has the tabs CA pool manager,
//     CA manager, Private certificate manager and Template manager;
//   - CA manager lists every CA and has Create CA, its first step Select
//     CA pool;
//   - Private certificate manager lists the issued certificates and has
//     Request a certificate: region, CA pool, an optional specific CA and
//     template, then Provide Certificate Signing Request (CSR) or Manually
//     enter details, whose key the console generates and hands back once
//     (Download private key);
//   - Create CA pool, Create CA and Create template follow Google's steps,
//     with the issuance policy's baseline values (base key usage, extended
//     key usage, policy identifiers, AIA OCSP servers, CA options and
//     additional extensions) and its allowed key algorithms and sizes.

// The X.509 steps Create CA pool and Create template share, in Google's
// order, each a section of the form.
const (
	pcaStepBaseKU = "Define base key usage"
	pcaStepEKU    = "Define extended key usage"
	pcaStepPolicy = "Define policy identifiers"
	pcaStepAIA    = "Add Authority information access (AIA) OCSP servers"
	pcaStepCAOpts = "Configure CA options"
	pcaStepExt    = "Configure additional extensions"
)

// pcaBaseUsages and pcaExtUsages are the key usage checkboxes, by form
// name, in the order the API's KeyUsageOptions declare them.
var pcaBaseUsages = [][2]string{
	{"kuDigitalSignature", "Digital signature"}, {"kuContentCommitment", "Content commitment"},
	{"kuKeyEncipherment", "Key encipherment"}, {"kuDataEncipherment", "Data encipherment"},
	{"kuKeyAgreement", "Key agreement"}, {"kuCertSign", "Certificate signing"}, {"kuCrlSign", "CRL signing"},
	{"kuEncipherOnly", "Encipher only"}, {"kuDecipherOnly", "Decipher only"},
}

var pcaExtUsages = [][2]string{
	{"ekuServerAuth", "Server authentication"}, {"ekuClientAuth", "Client authentication"},
	{"ekuCodeSigning", "Code signing"}, {"ekuEmailProtection", "Email protection"},
	{"ekuTimeStamping", "Time stamping"}, {"ekuOcspSigning", "OCSP signing"},
}

// privateCAX509Fields are the baseline (pool) or predefined (template)
// values' steps. showWhen, when set, shows them only while it holds.
func privateCAX509Fields(showWhen string) []console.Field {
	and := func(cond string) string {
		if showWhen == "" {
			return cond
		}
		if cond == "" {
			return showWhen
		}
		return showWhen + "&" + cond
	}
	out := []console.Field{{Name: "specifyBaseKeyUsage", Label: "Specify base key usages for certificates issued from this CA pool", Type: "checkbox",
		Section: pcaStepBaseKU, ShowWhen: and("")}}
	for _, u := range pcaBaseUsages {
		out = append(out, console.Field{Name: u[0], Label: u[1], Type: "checkbox", Section: pcaStepBaseKU, ShowWhen: and("specifyBaseKeyUsage")})
	}
	out = append(out, console.Field{Name: "writeExtendedKeyUsage", Label: "Write extended key usages for certificates issued from this CA pool", Type: "checkbox",
		Section: pcaStepEKU, ShowWhen: and(""), Help: "With no extended key usage, a certificate may be used for every key usage scenario."})
	for _, u := range pcaExtUsages {
		out = append(out, console.Field{Name: u[0], Label: u[1], Type: "checkbox", Section: pcaStepEKU, ShowWhen: and("writeExtendedKeyUsage")})
	}
	return append(out,
		console.Field{Name: "policyIds", Label: "Policy identifiers", Type: "text", Section: pcaStepPolicy, ShowWhen: and(""),
			Help: "Optional: comma-separated object identifiers, such as 1.2.3.4, written into the certificate policies extension."},
		console.Field{Name: "aiaOcspServers", Label: "Server URL", Type: console.GroupFieldType, Section: pcaStepAIA, ShowWhen: and(""), AddLabel: "Add item",
			Group: []console.Field{{Name: "url", Label: "Server URL", Type: "text"}},
			Help:  "Optional: the OCSP servers written into the authority information access extension."},
		console.Field{Name: "includeCaOptions", Label: "Include the configs to describe the CA X.509 extensions", Type: "checkbox",
			Section: pcaStepCAOpts, ShowWhen: and("")},
		console.Field{Name: "restrictToCa", Label: "Restrict issued certificates to be used only for CAs", Type: "checkbox",
			Section: pcaStepCAOpts, ShowWhen: and("includeCaOptions")},
		console.Field{Name: "includePathLength", Label: "Include the configs to describe the path length restriction X.509 extensions", Type: "checkbox",
			Section: pcaStepCAOpts, ShowWhen: and("")},
		console.Field{Name: "maxIssuerPathLength", Label: "Maximum issuer path length", Type: "text", Pattern: `^[0-9]{0,2}$`,
			Section: pcaStepCAOpts, ShowWhen: and("includePathLength"), Help: "How many CA certificates may follow this one in a chain."},
		console.Field{Name: "additionalExtensions", Label: "Additional extensions", Type: console.GroupFieldType, Section: pcaStepExt, ShowWhen: and(""),
			AddLabel: "Add item", Help: "Optional: extensions written as given.",
			Group: []console.Field{
				{Name: "oid", Label: "Object identifier", Type: "text"},
				{Name: "value", Label: "Value", Type: "text", Help: "Base64-encoded DER."},
				{Name: "critical", Label: "Extension is critical", Type: "checkbox"},
			}},
	)
}

// privateCAX509FromForm reads those steps; nil when nothing is set.
func privateCAX509FromForm(values map[string]string) (*privatecapb.X509Parameters, error) {
	p := &privatecapb.X509Parameters{}
	set := false
	on := func(name string) bool { return values[name] == "true" }
	if on("specifyBaseKeyUsage") {
		b := &privatecapb.KeyUsage_KeyUsageOptions{DigitalSignature: on("kuDigitalSignature"), ContentCommitment: on("kuContentCommitment"),
			KeyEncipherment: on("kuKeyEncipherment"), DataEncipherment: on("kuDataEncipherment"), KeyAgreement: on("kuKeyAgreement"),
			CertSign: on("kuCertSign"), CrlSign: on("kuCrlSign"), EncipherOnly: on("kuEncipherOnly"), DecipherOnly: on("kuDecipherOnly")}
		p.KeyUsage = &privatecapb.KeyUsage{BaseKeyUsage: b}
		set = true
	}
	if on("writeExtendedKeyUsage") {
		if p.KeyUsage == nil {
			p.KeyUsage = &privatecapb.KeyUsage{}
		}
		p.KeyUsage.ExtendedKeyUsage = &privatecapb.KeyUsage_ExtendedKeyUsageOptions{ServerAuth: on("ekuServerAuth"), ClientAuth: on("ekuClientAuth"),
			CodeSigning: on("ekuCodeSigning"), EmailProtection: on("ekuEmailProtection"), TimeStamping: on("ekuTimeStamping"), OcspSigning: on("ekuOcspSigning")}
		set = true
	}
	for _, s := range strings.Split(values["policyIds"], ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		o, err := privateCAOID(s)
		if err != nil {
			return nil, fmt.Errorf("policy identifiers: %v", err)
		}
		p.PolicyIds = append(p.PolicyIds, o)
		set = true
	}
	rows, err := console.ParseGroup(values["aiaOcspServers"])
	if err != nil {
		return nil, fmt.Errorf("AIA OCSP servers: %v", err)
	}
	for _, r := range rows {
		if u := strings.TrimSpace(r["url"]); u != "" {
			p.AiaOcspServers = append(p.AiaOcspServers, u)
			set = true
		}
	}
	if on("includeCaOptions") || on("includePathLength") {
		p.CaOptions = &privatecapb.X509Parameters_CaOptions{}
		if on("includeCaOptions") {
			p.CaOptions.IsCa = pcaPtr(on("restrictToCa"))
		}
		if on("includePathLength") {
			v := strings.TrimSpace(values["maxIssuerPathLength"])
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("maximum issuer path length: %q is not a number of 0 or more", v)
			}
			p.CaOptions.MaxIssuerPathLength = pcaPtr(int32(n))
		}
		set = true
	}
	rows, err = console.ParseGroup(values["additionalExtensions"])
	if err != nil {
		return nil, fmt.Errorf("additional extensions: %v", err)
	}
	for _, r := range rows {
		o, err := privateCAOID(strings.TrimSpace(r["oid"]))
		if err != nil {
			return nil, fmt.Errorf("additional extensions: object identifier: %v", err)
		}
		v, err := base64.StdEncoding.DecodeString(strings.TrimSpace(r["value"]))
		if err != nil {
			return nil, fmt.Errorf("additional extensions: the value of %s is not base64", r["oid"])
		}
		p.AdditionalExtensions = append(p.AdditionalExtensions, &privatecapb.X509Extension{ObjectId: o, Value: v, Critical: r["critical"] == "true"})
		set = true
	}
	if !set {
		return nil, nil
	}
	return p, nil
}

// pcaStepKeys is Create CA pool's allowed key algorithms step.
const pcaStepKeys = "Configure allowed key algorithms and sizes"

// pcaECTypes are the elliptic curve types an allowed key type names.
var pcaECTypes = []string{"ECDSA_P256", "ECDSA_P384", "EDDSA_25519"}

func privateCAKeyTypesField() console.Field {
	return console.Field{Name: "allowedKeyTypes", Label: "Allowed key algorithms", Type: console.GroupFieldType, Section: pcaStepKeys, AddLabel: "Add an item",
		Help: "Empty: every key algorithm and size is allowed. Each item allows RSA keys within the modulus sizes given, or the elliptic curve chosen.",
		Group: []console.Field{
			{Name: "type", Label: "Type", Type: "select", Options: []string{"RSA", "EC"}, OptionLabels: map[string]string{"RSA": "RSA", "EC": "Elliptic curve"}, Default: "RSA"},
			{Name: "minModulus", Label: "Minimum modulus size (bits)", Type: "text", Help: "RSA only; optional."},
			{Name: "maxModulus", Label: "Maximum modulus size (bits)", Type: "text", Help: "RSA only; optional."},
			{Name: "curve", Label: "Elliptic curve type", Type: "select", Options: pcaECTypes, Default: "ECDSA_P256",
				OptionLabels: map[string]string{"ECDSA_P256": "NIST P-256", "ECDSA_P384": "NIST P-384", "EDDSA_25519": "Ed25519"}},
		}}
}

// privateCAKeyTypesFromForm reads the allowed key algorithms.
func privateCAKeyTypesFromForm(v string) ([]*privatecapb.CaPool_IssuancePolicy_AllowedKeyType, error) {
	rows, err := console.ParseGroup(v)
	if err != nil {
		return nil, fmt.Errorf("allowed key algorithms: %v", err)
	}
	var out []*privatecapb.CaPool_IssuancePolicy_AllowedKeyType
	for _, r := range rows {
		switch r["type"] {
		case "", "RSA":
			rsaType := &privatecapb.CaPool_IssuancePolicy_AllowedKeyType_RsaKeyType{}
			for name, dst := range map[string]*int64{"minModulus": &rsaType.MinModulusSize, "maxModulus": &rsaType.MaxModulusSize} {
				if s := strings.TrimSpace(r[name]); s != "" {
					n, err := strconv.ParseInt(s, 10, 64)
					if err != nil || n < 0 {
						return nil, fmt.Errorf("allowed key algorithms: %q is not a modulus size in bits", s)
					}
					*dst = n
				}
			}
			out = append(out, &privatecapb.CaPool_IssuancePolicy_AllowedKeyType{KeyType: &privatecapb.CaPool_IssuancePolicy_AllowedKeyType_Rsa{Rsa: rsaType}})
		case "EC":
			alg, ok := privatecapb.CaPool_IssuancePolicy_AllowedKeyType_EcKeyType_EcSignatureAlgorithm_value[r["curve"]]
			if !ok {
				return nil, fmt.Errorf("allowed key algorithms: %q is not an elliptic curve type", r["curve"])
			}
			out = append(out, &privatecapb.CaPool_IssuancePolicy_AllowedKeyType{KeyType: &privatecapb.CaPool_IssuancePolicy_AllowedKeyType_EllipticCurve{
				EllipticCurve: &privatecapb.CaPool_IssuancePolicy_AllowedKeyType_EcKeyType{
					SignatureAlgorithm: privatecapb.CaPool_IssuancePolicy_AllowedKeyType_EcKeyType_EcSignatureAlgorithm(alg)}}})
		default:
			return nil, fmt.Errorf("allowed key algorithms: %q is not RSA or EC", r["type"])
		}
	}
	return out, nil
}

// privateCAKeyTypesText is the pool page's line for them.
func privateCAKeyTypesText(ts []*privatecapb.CaPool_IssuancePolicy_AllowedKeyType) string {
	if len(ts) == 0 {
		return "Any"
	}
	var out []string
	for _, t := range ts {
		if r := t.GetRsa(); r != nil {
			s := "RSA"
			if r.GetMinModulusSize() > 0 || r.GetMaxModulusSize() > 0 {
				s += fmt.Sprintf(" %d–%d bits", r.GetMinModulusSize(), r.GetMaxModulusSize())
			}
			out = append(out, s)
		} else {
			out = append(out, t.GetEllipticCurve().GetSignatureAlgorithm().String())
		}
	}
	return strings.Join(out, ", ")
}

// privateCAX509Text is a pool's baseline values or a template's predefined
// values as the page shows them.
func privateCAX509Text(p *privatecapb.X509Parameters) []console.Property {
	var base, ext, pol []string
	b := p.GetKeyUsage().GetBaseKeyUsage()
	for i, on := range []bool{b.GetDigitalSignature(), b.GetContentCommitment(), b.GetKeyEncipherment(), b.GetDataEncipherment(),
		b.GetKeyAgreement(), b.GetCertSign(), b.GetCrlSign(), b.GetEncipherOnly(), b.GetDecipherOnly()} {
		if on {
			base = append(base, pcaBaseUsages[i][1])
		}
	}
	e := p.GetKeyUsage().GetExtendedKeyUsage()
	for i, on := range []bool{e.GetServerAuth(), e.GetClientAuth(), e.GetCodeSigning(), e.GetEmailProtection(), e.GetTimeStamping(), e.GetOcspSigning()} {
		if on {
			ext = append(ext, pcaExtUsages[i][1])
		}
	}
	for _, o := range p.GetPolicyIds() {
		pol = append(pol, privateCAOIDText(o))
	}
	ca := "—"
	if co := p.GetCaOptions(); co != nil {
		var parts []string
		if co.IsCa != nil {
			parts = append(parts, fmt.Sprintf("CA: %v", co.GetIsCa()))
		}
		if co.MaxIssuerPathLength != nil {
			parts = append(parts, fmt.Sprintf("path length %d", co.GetMaxIssuerPathLength()))
		}
		ca = kmsOrDash(strings.Join(parts, ", "))
	}
	var extra []string
	for _, x := range p.GetAdditionalExtensions() {
		s := privateCAOIDText(x.GetObjectId())
		if x.GetCritical() {
			s += " (critical)"
		}
		extra = append(extra, s)
	}
	return []console.Property{
		{Label: "Base key usage", Value: kmsOrDash(strings.Join(base, ", "))},
		{Label: "Extended key usage", Value: kmsOrDash(strings.Join(ext, ", "))},
		{Label: "Policy identifiers", Value: kmsOrDash(strings.Join(pol, ", "))},
		{Label: "AIA OCSP servers", Value: kmsOrDash(strings.Join(p.GetAiaOcspServers(), ", "))},
		{Label: "CA options", Value: ca},
		{Label: "Additional extensions", Value: kmsOrDash(strings.Join(extra, ", "))},
	}
}

// privateCAPools are the project's CA pools' names, sorted.
func (p privateCAProvider) poolNames(project string) []string {
	s := p.srv()
	if s == nil || project == "" {
		return nil
	}
	var out []string
	for _, pool := range s.PoolsInProject(project) {
		out = append(out, pool.GetName())
	}
	sort.Strings(out)
	return out
}

// privateCACAsProvider is the CA manager tab: every certificate authority
// of the project's pools, with Create CA. A CA's page is the CA pool
// manager's, at the same path.
type privateCACAsProvider struct{ privateCAProvider }

func (privateCACAsProvider) ID() string    { return "privateca-cas" }
func (privateCACAsProvider) Title() string { return "CA manager" }

func (p privateCACAsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	api := p.api()
	if api == nil {
		return console.Listing{}, errors.New(privateCANotStarted)
	}
	out := console.Listing{Columns: []string{"CA pool", "Location", "Tier", "Type", "Expires"}, NameColumn: "Certificate authority",
		Noun: "certificate authorities", AlwaysStatus: true, RowsOpenable: true, Note: privateCANote}
	if project == "" {
		out.Prompt = "Certificate Authority Service lists CAs per project. Choose one in the toolbar."
		return out, nil
	}
	for _, pool := range p.srv().PoolsInProject(project) {
		resp, err := api.ListCertificateAuthorities(ctx, &privatecapb.ListCertificateAuthoritiesRequest{Parent: pool.GetName(), PageSize: 1000})
		if err != nil {
			continue
		}
		for _, ca := range resp.GetCertificateAuthorities() {
			id := lastSegment(ca.GetName())
			typ := "Root CA"
			if ca.GetType() == privatecapb.CertificateAuthority_SUBORDINATE {
				typ = "Subordinate CA"
			}
			out.Items = append(out.Items, console.Resource{Name: id, Status: ca.GetState().String(),
				Opens: []string{pool.GetName(), "certificateAuthorities", id},
				Fields: map[string]string{"CA pool": lastSegment(pool.GetName()), "Location": kmsLocation(pool.GetName()),
					"Tier": privateCATierText(pool.GetTier()), "Type": typ,
					"Expires": kmsTime(firstDescription(ca).GetSubjectDescription().GetNotAfterTime())}})
		}
	}
	out.Total = len(out.Items)
	return out, nil
}

func privateCATierText(t privatecapb.CaPool_Tier) string {
	switch t {
	case privatecapb.CaPool_ENTERPRISE:
		return "Enterprise"
	case privatecapb.CaPool_DEVOPS:
		return "DevOps"
	}
	return t.String()
}

func (privateCACAsProvider) CreateForm() (string, []console.Field) {
	return "Create CA", privateCACreateCAPageFields(nil)
}

func (privateCACAsProvider) CreateSubmit() string { return "Done" }

// CreateFormFor lists the project's pools in Select CA pool.
func (p privateCACAsProvider) CreateFormFor(_ context.Context, project string) []console.Field {
	return privateCACreateCAPageFields(p.poolNames(project))
}

// privateCACreateCAPageFields are Create CA's steps with Select CA pool
// first, as the CA manager's Create CA has them.
func privateCACreateCAPageFields(pools []string) []console.Field {
	f := console.Field{Name: "caPool", Label: "CA pool", Type: "select", Required: true, Options: pools, Section: "Select CA pool",
		Help: "Create a pool on the CA pool manager tab first if there is none."}
	if len(pools) > 0 {
		f.Default = pools[0]
	}
	return append([]console.Field{f}, privateCACreateCAFields()...)
}

func (p privateCACAsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	api := p.api()
	if api == nil {
		return "", errors.New(privateCANotStarted)
	}
	pool, err := privateCAPool(project, []string{strings.TrimSpace(values["caPool"])})
	if err != nil {
		return "", err
	}
	if err := privateCACreateCA(ctx, api, pool, values); err != nil {
		return "", err
	}
	return pool + "/certificateAuthorities/" + strings.TrimSpace(values["certificateAuthorityId"]), nil
}

// privateCACertsProvider is the Private certificate manager tab: every
// certificate the project's Enterprise pools issued, with Request a
// certificate.
type privateCACertsProvider struct{ privateCAProvider }

func (privateCACertsProvider) ID() string    { return "privateca-certs" }
func (privateCACertsProvider) Title() string { return "Private certificate manager" }

func (p privateCACertsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	api := p.api()
	if api == nil {
		return console.Listing{}, errors.New(privateCANotStarted)
	}
	out := console.Listing{Columns: []string{"Issuing CA", "CA pool", "Location", "Subject", "Expires"}, NameColumn: "Certificate",
		Noun: "certificates", AlwaysStatus: true, RowsOpenable: true,
		Note: "Only certificates of Enterprise-tier pools are stored, so only they are listed and can be revoked."}
	if project == "" {
		out.Prompt = "Certificate Authority Service lists certificates per project. Choose one in the toolbar."
		return out, nil
	}
	now := time.Now()
	for _, pool := range p.srv().PoolsInProject(project) {
		if pool.GetTier() != privatecapb.CaPool_ENTERPRISE {
			continue
		}
		resp, err := api.ListCertificates(ctx, &privatecapb.ListCertificatesRequest{Parent: pool.GetName(), PageSize: 1000})
		if err != nil {
			continue
		}
		for _, c := range resp.GetCertificates() {
			id := lastSegment(c.GetName())
			sd := c.GetCertificateDescription().GetSubjectDescription()
			status := "Active"
			switch {
			case c.GetRevocationDetails() != nil:
				status = "Revoked"
			case sd.GetNotAfterTime() != nil && sd.GetNotAfterTime().AsTime().Before(now):
				status = "Expired"
			}
			out.Items = append(out.Items, console.Resource{Name: id, Status: status,
				Opens: []string{pool.GetName(), "certificates", id},
				Fields: map[string]string{"Issuing CA": lastSegment(c.GetIssuerCertificateAuthority()), "CA pool": lastSegment(pool.GetName()),
					"Location": kmsLocation(pool.GetName()), "Subject": kmsOrDash(privateCASubject(sd.GetSubject())),
					"Expires": kmsTime(sd.GetNotAfterTime())}})
		}
	}
	out.Total = len(out.Items)
	return out, nil
}

func (privateCACertsProvider) CreateForm() (string, []console.Field) {
	return "Request a certificate", privateCARequestFields(nil, nil, nil)
}

func (privateCACertsProvider) CreateTitle() string  { return "Request a certificate" }
func (privateCACertsProvider) CreateSubmit() string { return "Generate certificate" }

// CreateFormFor lists the project's regions, pools, their CAs and the
// templates of each region.
func (p privateCACertsProvider) CreateFormFor(ctx context.Context, project string) []console.Field {
	pools := p.poolNames(project)
	cas, templates := map[string][]string{}, map[string][]string{}
	if api := p.api(); api != nil {
		for _, pool := range pools {
			cas[pool] = []string{""}
			if resp, err := api.ListCertificateAuthorities(ctx, &privatecapb.ListCertificateAuthoritiesRequest{Parent: pool, PageSize: 1000}); err == nil {
				for _, ca := range resp.GetCertificateAuthorities() {
					if ca.GetState() == privatecapb.CertificateAuthority_ENABLED {
						cas[pool] = append(cas[pool], lastSegment(ca.GetName()))
					}
				}
			}
		}
		for _, loc := range privateCALocations() {
			resp, err := api.ListCertificateTemplates(ctx, &privatecapb.ListCertificateTemplatesRequest{Parent: "projects/" + project + "/locations/" + loc, PageSize: 1000})
			if err != nil || len(resp.GetCertificateTemplates()) == 0 {
				continue
			}
			templates[loc] = []string{""}
			for _, t := range resp.GetCertificateTemplates() {
				templates[loc] = append(templates[loc], t.GetName())
			}
		}
	}
	return privateCARequestFields(pools, cas, templates)
}

// privateCAKeyAlgs are Manually enter details' generated key choices.
var privateCAKeyAlgs = []string{"RSA_2048", "RSA_3072", "RSA_4096", "EC_P256", "EC_P384"}

func privateCARequestFields(pools []string, cas, templates map[string][]string) []console.Field {
	regions := []string{}
	byRegion := map[string][]string{}
	for _, p := range pools {
		loc := kmsLocation(p)
		if _, ok := byRegion[loc]; !ok {
			regions = append(regions, loc)
		}
		byRegion[loc] = append(byRegion[loc], p)
	}
	poolLabels := map[string]string{}
	for _, p := range pools {
		poolLabels[p] = lastSegment(p)
	}
	var allTemplates []string
	tmplFor := map[string][]string{}
	tmplLabels := map[string]string{"": "None"}
	for _, r := range regions {
		tmplFor[r] = []string{""}
		for _, t := range templates[r] {
			if t != "" {
				tmplFor[r] = append(tmplFor[r], t)
				allTemplates = append(allTemplates, t)
				tmplLabels[t] = lastSegment(t)
			}
		}
	}
	var allCAs []string
	caLabels := map[string]string{"": "Any enabled CA of the pool"}
	seen := map[string]bool{"": true}
	for _, list := range cas {
		for _, c := range list {
			if !seen[c] {
				seen[c] = true
				allCAs = append(allCAs, c)
				caLabels[c] = c
			}
		}
	}
	sort.Strings(allCAs)
	sec1, sec2, sec3, sec4 := "Choose a CA pool", "Choose a request method", "Add certificate details", "Configure key size and algorithm"
	region := console.Field{Name: "location", Label: "Region", Type: "select", Required: true, Options: regions, Section: sec1,
		Help: "The region of the CA pool; it must match the pool's location."}
	if len(regions) > 0 {
		region.Default = regions[0]
	}
	pool := console.Field{Name: "caPool", Label: "CA pool", Type: "select", Required: true, Options: pools, OptionLabels: poolLabels,
		OptionsBy: "location", OptionsFor: byRegion, Section: sec1}
	if len(pools) > 0 {
		pool.Default = byRegion[region.Default][0]
	}
	return []console.Field{region, pool,
		{Name: "useSpecificCa", Label: "Use a specific CA from this CA pool", Type: "checkbox", Section: sec1,
			Help: "The certificate is then issued by that CA only, which makes it a dependency of the certificate."},
		{Name: "issuer", Label: "Certificate authority", Type: "select", Options: append([]string{""}, allCAs...), OptionLabels: caLabels,
			OptionsBy: "caPool", OptionsFor: cas, ShowWhen: "useSpecificCa", Section: sec1},
		{Name: "certificateTemplate", Label: "Certificate template", Type: "select", Options: append([]string{""}, allTemplates...), OptionLabels: tmplLabels,
			OptionsBy: "location", OptionsFor: tmplFor, Section: sec1, Help: "Optional: a template of the pool's region."},
		{Name: "method", Label: "Request method", Type: "radio", Options: []string{"csr", "manual"}, Default: "csr", Section: sec2,
			OptionLabels: map[string]string{"csr": "Provide Certificate Signing Request (CSR)", "manual": "Manually enter details"}},
		{Name: "certificateId", Label: "Certificate name", Type: "text", Pattern: `^([A-Za-z0-9_\-]{1,63})?$`, Section: sec3,
			Help: "Optional: empty is a generated name. A certificate cannot be deleted, so its name cannot be used again."},
		{Name: "lifetimeDays", Label: "Valid for (days)", Type: "text", Default: "30", Pattern: `^[0-9]{1,5}$`, Section: sec3},
		{Name: "pemCsr", Label: "Certificate CSR", Type: "textarea", ShowWhen: "method=csr", Upload: "Browse", Section: sec3,
			Help: "The PEM-encoded certificate signing request."},
		{Name: "domains", Label: "Add domain name", Type: console.GroupFieldType, ShowWhen: "method=manual", AddLabel: "Add item", Section: sec3,
			Group: []console.Field{{Name: "domain", Label: "Domain name", Type: "text"}}},
		{Name: "usage", Label: "Extended key usage", Type: "radio", Options: []string{"client_auth", "server_auth"}, Default: "server_auth",
			OptionLabels: map[string]string{"client_auth": "Client TLS", "server_auth": "Server TLS"}, ShowWhen: "method=manual", Section: sec3},
		{Name: "keyAlgorithm", Label: "Key size and algorithm", Type: "select", Options: privateCAKeyAlgs, Default: "RSA_2048", ShowWhen: "method=manual", Section: sec4,
			OptionLabels: map[string]string{"RSA_2048": "RSA 2048 bit", "RSA_3072": "RSA 3072 bit", "RSA_4096": "RSA 4096 bit",
				"EC_P256": "Elliptic curve P-256", "EC_P384": "Elliptic curve P-384"},
			Help: "The console generates the key pair; the private key is shown once on the certificate's page, to download."},
	}
}

// privateCAGeneratedKeys holds the private key Manually enter details made,
// by certificate name, for the certificate's page to offer once as
// Download private key: Certificate Authority Service never stores it.
var privateCAGeneratedKeys = struct {
	sync.Mutex
	m map[string]pcaGeneratedKey
}{m: map[string]pcaGeneratedKey{}}

type pcaGeneratedKey struct {
	pem     string
	expires time.Time
}

// privateCAKeyTTL is how long the certificate's page offers the key.
const privateCAKeyTTL = 15 * time.Minute

func rememberGeneratedKey(name, keyPEM string) {
	privateCAGeneratedKeys.Lock()
	defer privateCAGeneratedKeys.Unlock()
	now := time.Now()
	for k, v := range privateCAGeneratedKeys.m {
		if now.After(v.expires) {
			delete(privateCAGeneratedKeys.m, k)
		}
	}
	privateCAGeneratedKeys.m[name] = pcaGeneratedKey{pem: keyPEM, expires: now.Add(privateCAKeyTTL)}
}

func generatedKey(name string) string {
	privateCAGeneratedKeys.Lock()
	defer privateCAGeneratedKeys.Unlock()
	k, ok := privateCAGeneratedKeys.m[name]
	if !ok || time.Now().After(k.expires) {
		delete(privateCAGeneratedKeys.m, name)
		return ""
	}
	return k.pem
}

// generateKey makes Manually enter details' key pair: the private key as
// PKCS #8 PEM and the public key as PKIX PEM.
func generateKey(alg string) (privPEM, pubPEM string, err error) {
	var priv crypto.Signer
	switch alg {
	case "", "RSA_2048":
		priv, err = rsa.GenerateKey(rand.Reader, 2048)
	case "RSA_3072":
		priv, err = rsa.GenerateKey(rand.Reader, 3072)
	case "RSA_4096":
		priv, err = rsa.GenerateKey(rand.Reader, 4096)
	case "EC_P256":
		priv, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "EC_P384":
		priv, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	default:
		return "", "", fmt.Errorf("key size and algorithm: %q is not one of %s", alg, strings.Join(privateCAKeyAlgs, ", "))
	}
	if err != nil {
		return "", "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", "", err
	}
	pub, err := x509.MarshalPKIXPublicKey(priv.Public())
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})), nil
}

func (p privateCACertsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	api := p.api()
	if api == nil {
		return "", errors.New(privateCANotStarted)
	}
	pool, err := privateCAPool(project, []string{strings.TrimSpace(values["caPool"])})
	if err != nil {
		return "", err
	}
	if loc := strings.TrimSpace(values["location"]); loc != "" && loc != kmsLocation(pool) {
		return "", fmt.Errorf("the CA pool %s is in %s, not the region chosen, %s", lastSegment(pool), kmsLocation(pool), loc)
	}
	life, err := privateCADays("valid for", values["lifetimeDays"])
	if err != nil {
		return "", err
	}
	cert := &privatecapb.Certificate{Lifetime: life, CertificateTemplate: strings.TrimSpace(values["certificateTemplate"])}
	issuer := ""
	if values["useSpecificCa"] == "true" {
		issuer = strings.TrimSpace(values["issuer"])
	}
	var keyPEM string
	switch values["method"] {
	case "", "csr":
		csr := strings.TrimSpace(values["pemCsr"])
		if csr == "" {
			return "", fmt.Errorf("certificate CSR is required")
		}
		cert.CertificateConfig = &privatecapb.Certificate_PemCsr{PemCsr: csr + "\n"}
	case "manual":
		rows, err := console.ParseGroup(values["domains"])
		if err != nil {
			return "", fmt.Errorf("domain names: %v", err)
		}
		var dns []string
		for _, r := range rows {
			if d := strings.TrimSpace(r["domain"]); d != "" {
				dns = append(dns, d)
			}
		}
		if len(dns) == 0 {
			return "", fmt.Errorf("add at least one domain name")
		}
		priv, pub, err := generateKey(values["keyAlgorithm"])
		if err != nil {
			return "", err
		}
		keyPEM = priv
		eku := &privatecapb.KeyUsage_ExtendedKeyUsageOptions{ServerAuth: values["usage"] != "client_auth", ClientAuth: values["usage"] == "client_auth"}
		cert.CertificateConfig = &privatecapb.Certificate_Config{Config: &privatecapb.CertificateConfig{
			SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{
				Subject:        &privatecapb.Subject{CommonName: dns[0]},
				SubjectAltName: &privatecapb.SubjectAltNames{DnsNames: dns}},
			PublicKey: &privatecapb.PublicKey{Format: privatecapb.PublicKey_PEM, Key: []byte(pub)},
			X509Config: &privatecapb.X509Parameters{
				CaOptions: &privatecapb.X509Parameters_CaOptions{IsCa: new(bool)},
				KeyUsage: &privatecapb.KeyUsage{BaseKeyUsage: &privatecapb.KeyUsage_KeyUsageOptions{DigitalSignature: true, KeyEncipherment: true},
					ExtendedKeyUsage: eku}},
		}}
	default:
		return "", fmt.Errorf("request method: %q is not csr or manual", values["method"])
	}
	id := strings.TrimSpace(values["certificateId"])
	if id == "" {
		id = "cert-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	c, err := api.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: pool, CertificateId: id, Certificate: cert,
		IssuingCertificateAuthorityId: issuer})
	if err != nil {
		return "", err
	}
	if keyPEM != "" {
		rememberGeneratedKey(c.GetName(), keyPEM)
	}
	return c.GetName(), nil
}

var (
	_ console.ProjectCreateFormer = privateCACAsProvider{}
	_ console.ProjectCreateFormer = privateCACertsProvider{}
	_ console.CreateTitler        = privateCACertsProvider{}
)

// CreateTitle heads the CA manager's create page as its button.
func (privateCACAsProvider) CreateTitle() string { return "Create CA" }
