package main

import (
	"context"
	"encoding/asn1"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/privateca"
)

// privateCAProvider is the Certificate Authority Service page (#1834),
// Security > Certificate Authority Service in Google's console: a project's
// CA pools in every location, Create CA pool, and a pool's page with its
// certificate authorities and certificates, Create CA, Request certificate
// (from a CSR or from a subject and a public key), Edit labels, Delete pool
// and a Permissions tab. A CA's page offers Enable, Disable, Delete (with
// the grace period or without) and Undelete; a certificate's page Revoke
// and Edit labels.
//
// Every read and change is a call on the privateca.Server the gRPC and JSON
// transports serve, so what the page creates is what an SDK, gcloud and
// Terraform read.
//
// Paths are [pool resource name], [pool, "certificateAuthorities", id] and
// [pool, "certificates", id].
type privateCAProvider struct{ svc *privateCAService }

func (privateCAProvider) ID() string    { return "privateca" }
func (privateCAProvider) Title() string { return "Certificate Authority Service" }

const privateCANotStarted = "Certificate Authority Service has not started"

const privateCANote = "CA keys are generated and kept by this instance, unencrypted (docs/privateca.md); the certificates are real X.509, " +
	"verifiable against the CA's certificate. Long-running operations complete at once."

func (p privateCAProvider) srv() *privateca.Server { return p.svc.API() }

func (p privateCAProvider) api() *privateca.API {
	if s := p.srv(); s != nil {
		return s.API()
	}
	return nil
}

func (p privateCAProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s := p.srv()
	if s == nil {
		return console.Listing{}, errors.New(privateCANotStarted)
	}
	listing := console.Listing{
		Columns:    []string{"Location", "Tier", "CAs", "Labels"},
		NameColumn: "CA pool",
		Noun:       "CA pools",
		Note:       privateCANote,
	}
	if project == "" {
		listing.Prompt = "Certificate Authority Service lists CA pools per project. Choose one in the toolbar."
		return listing, nil
	}
	api := s.API()
	for _, pool := range s.PoolsInProject(project) {
		cas := "—"
		if resp, err := api.ListCertificateAuthorities(ctx, &privatecapb.ListCertificateAuthoritiesRequest{Parent: pool.GetName(), PageSize: 1000}); err == nil {
			cas = strconv.Itoa(len(resp.GetCertificateAuthorities()))
		}
		listing.Items = append(listing.Items, console.Resource{
			Name: pool.GetName(),
			Fields: map[string]string{
				"Location": kmsLocation(pool.GetName()),
				"Tier":     pool.GetTier().String(),
				"CAs":      cas,
				"Labels":   kmsOrDash(labelsText(pool.GetLabels())),
			},
		})
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

// privateCALocations are the locations the service lists.
func privateCALocations() []string {
	out := append([]string(nil), privateca.Regions...)
	sort.Strings(out)
	return out
}

const privateCAIDPattern = `^[A-Za-z0-9_\-]{1,63}$`

// CreateSubmit implements console.CreateSubmitter: the create flow ends
// in Done, as docs.cloud.google.com/certificate-authority-service/docs/creating-ca-pool words it (#1933).
func (privateCAProvider) CreateSubmit() string { return "Done" }

// CreateTitle implements console.CreateTitler: the page the create button
// opens is headed Create CA pool (#1933).
func (privateCAProvider) CreateTitle() string { return "Create CA pool" }

func (privateCAProvider) CreateForm() (string, []console.Field) {
	// Google's steps (creating-ca-pool, read 2026-11-22): name, region and
	// tier; allowed key algorithms and sizes; certificate request methods;
	// publishing options; then the baseline values' steps. The issuance
	// policy's lifetime, identity constraints and passthrough extensions,
	// and CMEK and labels, follow as CloudBurrow's own sections: Google's
	// pool wizard does not show them in the docs.
	// UNVERIFIED: docs re-read 2026-11-22, still unanswered: https://docs.cloud.google.com/certificate-authority-service/docs/creating-ca-pool
	const first, methods, publish, more = "Name, region and tier", "Configure certificate request methods", "Configure publishing options", "Issuance policy and labels"
	fields := []console.Field{
		{Name: "caPoolId", Label: "Name", Type: "text", Required: true, Pattern: privateCAIDPattern, Section: first,
			Help: "Unique for the region: 1 to 63 letters, digits, hyphens and underscores. The name, region and tier cannot be changed later."},
		{Name: "location", Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: privateCALocations(), Section: first},
		{Name: "tier", Label: "Tier", Type: "radio", Required: true, Default: "ENTERPRISE", Options: []string{"ENTERPRISE", "DEVOPS"}, Section: first,
			OptionLabels: map[string]string{"ENTERPRISE": "Enterprise", "DEVOPS": "DevOps"},
			Help:         "Enterprise stores the certificates it issues, which can then be listed and revoked; DevOps does not store them."},
		privateCAKeyTypesField(),
		{Name: "allowCsr", Label: "Allow certificate requests from a CSR", Type: "checkbox", Default: "true", Section: methods},
		{Name: "allowConfig", Label: "Allow certificate requests from a config", Type: "checkbox", Default: "true", Section: methods},
		{Name: "publishCaCert", Label: "Publish CA certificate to GCS bucket for CAs in this pool", Type: "checkbox", Default: "true", Section: publish,
			Help: "Each CA's certificate is published, at its bucket when it names one, and its URL put in the certificates it issues."},
		{Name: "publishCrl", Label: "Publish CRL to GCS bucket for CAs in this pool", Type: "checkbox", Default: "true", Section: publish,
			Help: "Each CA's CRL is published, at its bucket when it names one, and its URL put in the certificates it issues; a new CRL daily and within 15 minutes of a revocation."},
		{Name: "encodingFormat", Label: "Encoding format", Type: "select", Default: "PEM", Options: []string{"PEM", "DER"}, Section: publish},
	}
	fields = append(fields, privateCAX509Fields("")...)
	return "Create pool", append(fields,
		console.Field{Name: "maximumLifetimeDays", Label: "Maximum certificate lifetime (days)", Type: "text", Pattern: `^[0-9]{0,5}$`, Section: more,
			Help: "Optional: the issuance policy's maximum_lifetime. A longer request is shortened to it."},
		console.Field{Name: "allowRequesterNotBefore", Label: "Allow a requester-specified not-before time", Type: "checkbox", Section: more,
			Help: "The issuance policy's allow_requester_specified_not_before_time: a request may then set its certificate's not-before time."},
		console.Field{Name: "allowSubjectPassthrough", Label: "Copy subject from the certificate requests to the certificate", Type: "checkbox", Default: "true", Section: more},
		console.Field{Name: "allowSansPassthrough", Label: "Copy subject alternative names (SAN) from the certificate requests to the certificate", Type: "checkbox", Default: "true", Section: more},
		console.Field{Name: "celExpression", Label: "CEL expression", Type: "textarea", Help: privateCACELHelp, Section: more},
		console.Field{Name: "passthroughExtensions", Label: "Passthrough extensions", Type: "text", Help: privateCAExtensionsHelp, Section: more},
		console.Field{Name: "encryptionKey", Label: "Customer-managed encryption key (CMEK)", Type: "text", Section: more,
			Help: "Optional, Enterprise tier only: a Cloud KMS ENCRYPT_DECRYPT key in the pool's region, projects/…/locations/…/keyRings/…/cryptoKeys/…, that encrypts the pool's certificates at rest."},
		withSection(labelsField(nil, ""), more),
	)
}

func privateCADays(field, v string) (*durationpb.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return nil, fmt.Errorf("%s: %q is not a positive number of days", field, v)
	}
	return durationpb.New(time.Duration(n) * 24 * time.Hour), nil
}

func (p privateCAProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	api := p.api()
	if api == nil {
		return "", errors.New(privateCANotStarted)
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a CA pool")
	}
	tier, ok := privatecapb.CaPool_Tier_value[values["tier"]]
	if !ok {
		return "", fmt.Errorf("tier: %q is not ENTERPRISE or DEVOPS", values["tier"])
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return "", err
	}
	life, err := privateCADays("maximum certificate lifetime", values["maximumLifetimeDays"])
	if err != nil {
		return "", err
	}
	pool := &privatecapb.CaPool{Tier: privatecapb.CaPool_Tier(tier), Labels: labels}
	der := values["encodingFormat"] == "DER"
	if values["publishCaCert"] == "true" || values["publishCrl"] == "true" || der {
		pool.PublishingOptions = &privatecapb.CaPool_PublishingOptions{PublishCaCert: values["publishCaCert"] == "true", PublishCrl: values["publishCrl"] == "true"}
		if der {
			pool.PublishingOptions.EncodingFormat = privatecapb.CaPool_PublishingOptions_DER
		}
	}
	keyTypes, err := privateCAKeyTypesFromForm(values["allowedKeyTypes"])
	if err != nil {
		return "", err
	}
	baseline, err := privateCAX509FromForm(values)
	if err != nil {
		return "", err
	}
	if k := strings.TrimSpace(values["encryptionKey"]); k != "" {
		pool.EncryptionSpec = &privatecapb.EncryptionSpec{CloudKmsKey: k}
	}
	ext, err := privateCAParseExtensions(values["passthroughExtensions"])
	if err != nil {
		return "", err
	}
	ident := privateCAIdentity(values)
	notBefore := values["allowRequesterNotBefore"] == "true"
	csr, cfg := values["allowCsr"] != "false", values["allowConfig"] != "false"
	if life != nil || !csr || !cfg || ext != nil || ident != nil || notBefore || keyTypes != nil || baseline != nil {
		pool.IssuancePolicy = &privatecapb.CaPool_IssuancePolicy{MaximumLifetime: life, PassthroughExtensions: ext,
			IdentityConstraints: ident, AllowRequesterSpecifiedNotBeforeTime: notBefore, AllowedKeyTypes: keyTypes, BaselineValues: baseline}
		if !csr || !cfg {
			pool.IssuancePolicy.AllowedIssuanceModes = &privatecapb.CaPool_IssuancePolicy_IssuanceModes{
				AllowCsrBasedIssuance: csr, AllowConfigBasedIssuance: cfg}
		}
	}
	op, err := api.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{
		Parent: "projects/" + project + "/locations/" + strings.TrimSpace(values["location"]), CaPoolId: strings.TrimSpace(values["caPoolId"]), CaPool: pool})
	if err != nil {
		return "", err
	}
	out := &privatecapb.CaPool{}
	if err := op.GetResponse().UnmarshalTo(out); err != nil {
		return "", err
	}
	return out.GetName(), nil
}

// privateCAPool checks that a path's pool belongs to the project the page
// is scoped to, so a hand-written URL cannot open another project's pool.
func privateCAPool(project string, path []string) (string, error) {
	if len(path) == 0 {
		return "", fmt.Errorf("no CA pool")
	}
	if err := kmsOwned(project, path[0]); err != nil {
		return "", err
	}
	if !strings.Contains(path[0], "/caPools/") {
		return "", fmt.Errorf("%s is not a CA pool", path[0])
	}
	return path[0], nil
}

func (p privateCAProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 3 || len(path) == 2 {
		return console.DeeperThan(3, path), nil
	}
	api := p.api()
	if api == nil {
		return console.Detail{Unavailable: privateCANotStarted}, nil
	}
	pool, err := privateCAPool(project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	if len(path) == 3 {
		switch path[1] {
		case "certificateAuthorities":
			return p.caDetail(ctx, pool+"/certificateAuthorities/"+path[2])
		case "certificates":
			return p.certDetail(ctx, pool+"/certificates/"+path[2])
		}
		return console.DeeperThan(1, path), nil
	}
	return p.poolDetail(ctx, pool)
}

func privateCAPolicyText(pol *privatecapb.CaPool_IssuancePolicy) []console.Property {
	modes := "CSR and config"
	if m := pol.GetAllowedIssuanceModes(); m != nil {
		switch {
		case m.GetAllowCsrBasedIssuance() && m.GetAllowConfigBasedIssuance():
		case m.GetAllowCsrBasedIssuance():
			modes = "CSR only"
		case m.GetAllowConfigBasedIssuance():
			modes = "Config only"
		default:
			modes = "None"
		}
	}
	life := "No maximum"
	if d := pol.GetMaximumLifetime(); d != nil {
		life = d.AsDuration().String()
	}
	ic := pol.GetIdentityConstraints()
	return []console.Property{
		{Label: "Allowed request types", Value: modes},
		{Label: "Maximum certificate lifetime", Value: life},
		{Label: "Requester-specified not-before time", Value: privateCAAllowed(pol.GetAllowRequesterSpecifiedNotBeforeTime())},
		{Label: "Request's subject allowed", Value: privateCAAllowed(ic == nil || ic.GetAllowSubjectPassthrough())},
		{Label: "Request's SANs allowed", Value: privateCAAllowed(ic == nil || ic.GetAllowSubjectAltNamesPassthrough())},
		{Label: "Identity CEL expression", Value: kmsOrDash(ic.GetCelExpression().GetExpression())},
		{Label: "Passthrough extensions", Value: kmsOrDash(privateCAExtensionsText(pol.GetPassthroughExtensions()))},
	}
}

func privateCAAllowed(b bool) string {
	if b {
		return "Allowed"
	}
	return "Not allowed"
}

func (p privateCAProvider) poolDetail(ctx context.Context, name string) (console.Detail, error) {
	api := p.api()
	pool, err := api.GetCaPool(ctx, &privatecapb.GetCaPoolRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the CA pool: " + consoleErr(err)}, nil
	}
	cas := console.Listing{Columns: []string{"Type", "Subject", "Expires"}, NameColumn: "Certificate authority", Noun: "certificate authorities"}
	if resp, err := api.ListCertificateAuthorities(ctx, &privatecapb.ListCertificateAuthoritiesRequest{Parent: name, PageSize: 1000}); err != nil {
		cas.Unavailable = "cannot list the CAs: " + consoleErr(err)
	} else {
		for _, ca := range resp.GetCertificateAuthorities() {
			id := ca.GetName()[strings.LastIndex(ca.GetName(), "/")+1:]
			d := firstDescription(ca)
			cas.Items = append(cas.Items, console.Resource{Name: id, Status: ca.GetState().String(),
				Opens: []string{name, "certificateAuthorities", id},
				Fields: map[string]string{"Type": ca.GetType().String(),
					"Subject": kmsOrDash(privateCASubject(d.GetSubjectDescription().GetSubject())),
					"Expires": kmsTime(d.GetSubjectDescription().GetNotAfterTime())}})
		}
	}
	cas.Total = len(cas.Items)
	certs := console.Listing{Columns: []string{"Issuer", "Subject", "Expires", "Revoked"}, NameColumn: "Certificate", Noun: "certificates"}
	if pool.GetTier() == privatecapb.CaPool_DEVOPS {
		certs.Prompt = "A DevOps pool does not store the certificates it issues, so none are listed and none can be revoked."
	} else if resp, err := api.ListCertificates(ctx, &privatecapb.ListCertificatesRequest{Parent: name, PageSize: 1000}); err != nil {
		certs.Unavailable = "cannot list the certificates: " + consoleErr(err)
	} else {
		for _, c := range resp.GetCertificates() {
			id := c.GetName()[strings.LastIndex(c.GetName(), "/")+1:]
			revoked := "—"
			if r := c.GetRevocationDetails(); r != nil {
				revoked = r.GetRevocationState().String()
			}
			issuer := c.GetIssuerCertificateAuthority()
			certs.Items = append(certs.Items, console.Resource{Name: id,
				Opens: []string{name, "certificates", id},
				Fields: map[string]string{"Issuer": issuer[strings.LastIndex(issuer, "/")+1:],
					"Subject": kmsOrDash(privateCASubject(c.GetCertificateDescription().GetSubjectDescription().GetSubject())),
					"Expires": kmsTime(c.GetCertificateDescription().GetSubjectDescription().GetNotAfterTime()), "Revoked": revoked}})
		}
	}
	certs.Total = len(certs.Items)
	var anchors strings.Builder
	if resp, err := api.FetchCaCerts(ctx, &privatecapb.FetchCaCertsRequest{CaPool: name}); err == nil {
		for _, chain := range resp.GetCaCerts() {
			for _, c := range chain.GetCertificates() {
				anchors.WriteString(c)
				if !strings.HasSuffix(c, "\n") {
					anchors.WriteString("\n")
				}
			}
		}
	}
	if anchors.Len() == 0 {
		anchors.WriteString("The pool has no enabled, disabled or staged CA, so it has no trust anchors.")
	}
	return console.Detail{
		Summary: []console.Property{
			{Label: "Region", Value: kmsLocation(name)},
			{Label: "Tier", Value: pool.GetTier().String()},
		},
		Sections: []console.Section{
			{ID: "authorities", Label: "Certificate authorities", Listing: cas},
			{ID: "certificates", Label: "Certificates", Listing: certs},
			{ID: "configuration", Label: "Configuration", Kind: console.KindProperties, Note: privateCANote, Groups: []console.PropertyGroup{
				{Heading: "CA pool", Properties: []console.Property{
					{Label: "Resource name", Value: name},
					{Label: "Customer-managed encryption key", Value: kmsOrDash(pool.GetEncryptionSpec().GetCloudKmsKey())},
					{Label: "Publishes CA certificates", Value: fmt.Sprint(pool.GetPublishingOptions().GetPublishCaCert())},
					{Label: "Publishes CRLs", Value: fmt.Sprint(pool.GetPublishingOptions().GetPublishCrl())},
					{Label: "Labels", Value: kmsOrDash(labelsText(pool.GetLabels()))}}},
				{Heading: "Issuance policy", Properties: append(privateCAPolicyText(pool.GetIssuancePolicy()),
					console.Property{Label: "Allowed key algorithms", Value: privateCAKeyTypesText(pool.GetIssuancePolicy().GetAllowedKeyTypes())},
					console.Property{Label: "Encoding format", Value: privateCAEncoding(pool)})},
				{Heading: "Baseline values", Properties: privateCAX509Text(pool.GetIssuancePolicy().GetBaselineValues())}}},
			{ID: "cacerts", Label: "CA certificates", Kind: console.KindText, Text: anchors.String(),
				Note: "The pool's trust anchors, as FetchCaCerts answers them (gcloud privateca pools get-ca-certs)."},
		},
	}, nil
}

// privateCAEncoding is the pool's publishing encoding format.
func privateCAEncoding(pool *privatecapb.CaPool) string {
	if pool.GetPublishingOptions().GetEncodingFormat() == privatecapb.CaPool_PublishingOptions_DER {
		return "DER"
	}
	return "PEM"
}

func firstDescription(ca *privatecapb.CertificateAuthority) *privatecapb.CertificateDescription {
	if ds := ca.GetCaCertificateDescriptions(); len(ds) > 0 {
		return ds[0]
	}
	return nil
}

// privateCASubject is a subject as an RFC 4514 string, its common name
// first, or its RDN sequence in order.
func privateCASubject(s *privatecapb.Subject) string {
	if s == nil {
		return ""
	}
	// A subject made from an RDN sequence is shown in the sequence's
	// order, the order its certificate encodes.
	if seq := s.GetRdnSequence(); len(seq) > 0 {
		return strings.ReplaceAll(privateCARDNText(seq), "\n", ",")
	}
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	add("CN", s.GetCommonName())
	add("OU", s.GetOrganizationalUnit())
	add("O", s.GetOrganization())
	add("L", s.GetLocality())
	add("ST", s.GetProvince())
	add("C", s.GetCountryCode())
	return strings.Join(parts, ",")
}

func (p privateCAProvider) caDetail(ctx context.Context, name string) (console.Detail, error) {
	ca, err := p.api().GetCertificateAuthority(ctx, &privatecapb.GetCertificateAuthorityRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the CA: " + consoleErr(err)}, nil
	}
	d := firstDescription(ca)
	props := []console.Property{
		{Label: "Resource name", Value: name},
		{Label: "Type", Value: ca.GetType().String()},
		{Label: "State", Value: ca.GetState().String()},
		{Label: "Subject", Value: kmsOrDash(privateCASubject(d.GetSubjectDescription().GetSubject()))},
		privateCAKeyProperty(ca.GetKeySpec()),
		{Label: "Publishing bucket", Value: kmsOrDash(ca.GetGcsBucket())},
		{Label: "Not before", Value: kmsTime(d.GetSubjectDescription().GetNotBeforeTime())},
		{Label: "Not after", Value: kmsTime(d.GetSubjectDescription().GetNotAfterTime())},
		{Label: "Labels", Value: kmsOrDash(labelsText(ca.GetLabels()))},
	}
	if ca.GetSubordinateConfig() != nil {
		issuer := ca.GetSubordinateConfig().GetCertificateAuthority()
		if issuer == "" {
			issuer = "A third-party issuer (its PEM chain)"
		}
		props = append(props, console.Property{Label: "Issuer", Value: issuer})
	}
	if u := ca.GetAccessUrls(); u.GetCaCertificateAccessUrl() != "" || len(u.GetCrlAccessUrls()) > 0 {
		props = append(props, console.Property{Label: "CA certificate URL", Value: kmsOrDash(u.GetCaCertificateAccessUrl())},
			console.Property{Label: "CRL URLs", Value: kmsOrDash(strings.Join(u.GetCrlAccessUrls(), ", "))})
	}
	if ca.GetDeleteTime() != nil {
		props = append(props, console.Property{Label: "Deleted", Value: kmsTime(ca.GetDeleteTime())},
			console.Property{Label: "Purged after", Value: kmsTime(ca.GetExpireTime())})
	}
	sections := []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Certificate authority", Properties: props}}, Note: privateCANote},
		{ID: "pem", Label: "CA certificate chain", Kind: console.KindText, Text: kmsOrDash(strings.Join(ca.GetPemCaCertificates(), ""))},
	}
	if ca.GetState() == privatecapb.CertificateAuthority_AWAITING_USER_ACTIVATION {
		csr := "—"
		if resp, err := p.api().FetchCertificateAuthorityCsr(ctx, &privatecapb.FetchCertificateAuthorityCsrRequest{Name: name}); err == nil {
			csr = resp.GetPemCsr()
		}
		sections = append(sections, console.Section{ID: "csr", Label: "CSR", Kind: console.KindText, Text: csr,
			Note: "Have the issuer sign this CSR as a CA certificate, then Activate with the certificate and the issuer's chain."})
	}
	crls := console.Listing{Columns: []string{"Sequence", "Revoked", "State", "Updated", "Labels"}, NameColumn: "Revocation list", Noun: "revocation lists"}
	var crlText strings.Builder
	if resp, err := p.api().ListCertificateRevocationLists(ctx, &privatecapb.ListCertificateRevocationListsRequest{Parent: name, PageSize: 1000}); err != nil {
		crls.Unavailable = "cannot list the CRLs: " + consoleErr(err)
	} else {
		for _, c := range resp.GetCertificateRevocationLists() {
			crls.Items = append(crls.Items, console.Resource{Name: c.GetName()[strings.LastIndex(c.GetName(), "/")+1:], Fields: map[string]string{
				"Sequence": strconv.FormatInt(c.GetSequenceNumber(), 10), "Revoked": strconv.Itoa(len(c.GetRevokedCertificates())),
				"State": c.GetState().String(), "Updated": kmsTime(c.GetUpdateTime()), "Labels": kmsOrDash(labelsText(c.GetLabels()))}})
			crlText.WriteString(c.GetPemCrl())
		}
	}
	crls.Total = len(crls.Items)
	sections = append(sections, console.Section{ID: "crls", Label: "Revocation lists", Listing: crls},
		console.Section{ID: "crlpem", Label: "Current CRL", Kind: console.KindText, Text: kmsOrDash(crlText.String()),
			Note: "Regenerated on each revocation. With the pool publishing CRLs it is also served at the CA's CRL URL."})
	return console.Detail{
		Summary: []console.Property{
			{Label: "State", Value: ca.GetState().String()},
			{Label: "Created", Value: kmsTime(ca.GetCreateTime())},
		},
		Sections: sections,
	}, nil
}

func (p privateCAProvider) certDetail(ctx context.Context, name string) (console.Detail, error) {
	c, err := p.api().GetCertificate(ctx, &privatecapb.GetCertificateRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the certificate: " + consoleErr(err)}, nil
	}
	sd := c.GetCertificateDescription().GetSubjectDescription()
	props := []console.Property{
		{Label: "Resource name", Value: name},
		{Label: "Issuer", Value: c.GetIssuerCertificateAuthority()},
		{Label: "Subject", Value: kmsOrDash(privateCASubject(sd.GetSubject()))},
		{Label: "Subject alternative names", Value: kmsOrDash(strings.Join(sd.GetSubjectAltName().GetDnsNames(), ", "))},
		{Label: "Serial number", Value: kmsOrDash(sd.GetHexSerialNumber())},
		{Label: "Subject mode", Value: kmsOrDash(privateCASubjectMode(c.GetSubjectMode()))},
		{Label: "Not before", Value: kmsTime(sd.GetNotBeforeTime())},
		{Label: "Not after", Value: kmsTime(sd.GetNotAfterTime())},
		{Label: "Labels", Value: kmsOrDash(labelsText(c.GetLabels()))},
	}
	status := "Active"
	if r := c.GetRevocationDetails(); r != nil {
		status = "Revoked"
		props = append(props, console.Property{Label: "Revocation reason", Value: r.GetRevocationState().String()},
			console.Property{Label: "Revoked", Value: kmsTime(r.GetRevocationTime())})
	}
	d := console.Detail{
		Summary: []console.Property{
			{Label: "Status", Value: status},
			{Label: "Created", Value: kmsTime(c.GetCreateTime())},
		},
		Sections: []console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Certificate", Properties: props}}},
			{ID: "pem", Label: "Certificate", Kind: console.KindText, Text: c.GetPemCertificate(), Download: lastSegment(name) + ".crt"},
			{ID: "chain", Label: "Certificate chain", Kind: console.KindText, Text: kmsOrDash(strings.Join(c.GetPemCertificateChain(), "")),
				Download: lastSegment(name) + "-chain.crt"},
		},
	}
	// Manually enter details' generated key, offered as Download private
	// key for a while after the request (#2144): CAS never stores it.
	if key := generatedKey(name); key != "" {
		d.Sections = append(d.Sections, console.Section{ID: "privatekey", Label: "Private key", Kind: console.KindText, Text: key,
			Download: lastSegment(name) + ".key",
			Note:     "The key the console generated for this certificate. Download it now: Certificate Authority Service does not keep it, and this page stops offering it 15 minutes after the request."})
	}
	return d, nil
}

// privateCAAlgorithms are the CA key algorithms, as Create CA offers them.
func privateCAAlgorithms() []string {
	var out []string
	for i := int32(1); ; i++ {
		n, ok := privatecapb.CertificateAuthority_SignHashAlgorithm_name[i]
		if !ok {
			break
		}
		out = append(out, n)
	}
	return out
}

func privateCARevocationReasons() []string {
	var out []string
	for i := int32(0); ; i++ {
		n, ok := privatecapb.RevocationReason_name[i]
		if !ok {
			break
		}
		out = append(out, n)
	}
	return out
}

func (p privateCAProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil {
		return nil
	}
	if _, err := privateCAPool(project, path); err != nil {
		return nil
	}
	switch {
	case len(path) == 1:
		var labels map[string]string
		if pool, err := p.api().GetCaPool(ctx, &privatecapb.GetCaPoolRequest{Name: path[0]}); err == nil {
			labels = pool.GetLabels()
		}
		return []console.Action{
			{ID: "createca", Label: "Create CA", Fields: privateCACreateCAFields()},
			{ID: "requestcsr", Label: "Request certificate from a CSR", Fields: []console.Field{
				{Name: "certificateId", Label: "Certificate ID", Type: "text", Pattern: `^([A-Za-z0-9_\-]{1,63})?$`,
					Help: "Required in an Enterprise pool; in a DevOps pool, empty is generated."},
				{Name: "pemCsr", Label: "Certificate signing request (PEM)", Type: "textarea", Required: true},
				{Name: "lifetimeDays", Label: "Validity (days)", Type: "text", Default: "30", Pattern: `^[0-9]{1,5}$`},
				privateCASubjectModeField(), privateCANotBeforeField(),
				{Name: "certificateTemplate", Label: "Certificate template", Type: "text",
					Help: "Optional: projects/…/locations/…/certificateTemplates/…, in the pool's region."},
				{Name: "issuer", Label: "Issuing CA ID", Type: "text", Pattern: `^([A-Za-z0-9_\-]{1,63})?$`, Help: "Optional: empty is any enabled CA of the pool."},
			}},
			{ID: "requestconfig", Label: "Request certificate from a config", Fields: []console.Field{
				{Name: "certificateId", Label: "Certificate ID", Type: "text", Pattern: `^([A-Za-z0-9_\-]{1,63})?$`,
					Help: "Required in an Enterprise pool; in a DevOps pool, empty is generated."},
				{Name: "commonName", Label: "Common name", Type: "text"},
				{Name: "dnsNames", Label: "DNS names", Type: "text", Help: "Comma-separated subject alternative names."},
				{Name: "customSans", Label: "Custom SANs", Type: "textarea",
					Help: "Optional: one OID=text per line, each written as an otherName SAN whose value is the text as a UTF8String (#1961)."},
				{Name: "subjectKeyId", Label: "Subject key ID", Type: "text", Pattern: `^([0-9a-f]{2})*$`,
					Help: "Optional: lowercase hexadecimal; empty is the SHA-1 of the public key."},
				{Name: "rdnSequence", Label: "Subject RDN sequence", Type: "textarea", Help: privateCARDNHelp},
				{Name: "publicKey", Label: "Public key (PEM)", Type: "textarea", Required: true},
				{Name: "usage", Label: "Extended key usage", Type: "select", Default: "server_auth", Options: []string{"server_auth", "client_auth", "server_auth,client_auth"}},
				{Name: "lifetimeDays", Label: "Validity (days)", Type: "text", Default: "30", Pattern: `^[0-9]{1,5}$`},
				privateCASubjectModeField(), privateCANotBeforeField(),
				{Name: "certificateTemplate", Label: "Certificate template", Type: "text",
					Help: "Optional: projects/…/locations/…/certificateTemplates/…, in the pool's region."},
				{Name: "issuer", Label: "Issuing CA ID", Type: "text", Pattern: `^([A-Za-z0-9_\-]{1,63})?$`, Help: "Optional: empty is any enabled CA of the pool."},
			}},
			{ID: "labels", Label: "Edit labels", Fields: []console.Field{labelsField(labels, "")}},
			{ID: "deletepool", Label: "Delete pool", Destructive: true, Leaves: true},
		}
	case len(path) == 3 && path[1] == "certificateAuthorities":
		ca, err := p.api().GetCertificateAuthority(ctx, &privatecapb.GetCertificateAuthorityRequest{Name: path[0] + "/certificateAuthorities/" + path[2]})
		if err != nil {
			return nil
		}
		var out []console.Action
		switch ca.GetState() {
		case privatecapb.CertificateAuthority_STAGED, privatecapb.CertificateAuthority_DISABLED:
			out = append(out, console.Action{ID: "enable", Label: "Enable", Submit: "Confirm", ConfirmStyle: console.DeleteConfirmClick, Confirm: "The CA issues certificates once it is enabled."})
		}
		switch ca.GetState() {
		case privatecapb.CertificateAuthority_STAGED, privatecapb.CertificateAuthority_ENABLED:
			out = append(out, console.Action{ID: "disable", Label: "Disable", Submit: "Confirm", ConfirmStyle: console.DeleteConfirmClick,
				Confirm: "A disabled CA issues no certificates; the pool's other enabled CAs still do."})
		}
		if ca.GetState() == privatecapb.CertificateAuthority_AWAITING_USER_ACTIVATION {
			out = append(out, privateCAActivateAction())
		}
		switch ca.GetState() {
		case privatecapb.CertificateAuthority_DISABLED, privatecapb.CertificateAuthority_STAGED, privatecapb.CertificateAuthority_AWAITING_USER_ACTIVATION:
			// Google's Delete Certificate Authority dialog, ending in
			// Confirm (deleting-certificate-authorities, read 2026-11-22).
			out = append(out, console.Action{ID: "delete", Label: "Delete", Destructive: true, Title: "Delete Certificate Authority", Submit: "Confirm",
				Fields: []console.Field{
					{Name: "ignoreActiveCertificates", Label: "Delete this CA, even if there are active certificates", Type: "checkbox",
						Help: "Systems that rely on its certificates may stop working."},
					{Name: "skipGracePeriod", Label: "Skip the 30 day grace period and delete this CA immediately", Type: "checkbox",
						Help: "Recommended only outside production: the CA cannot then be restored."}}})
		case privatecapb.CertificateAuthority_DELETED:
			out = append(out, console.Action{ID: "undelete", Label: "Restore", Submit: "Confirm", ConfirmStyle: console.DeleteConfirmClick,
				Confirm: "The CA is restored to the Disabled state; enable it to issue certificates again."})
		}
		if ca.GetState() != privatecapb.CertificateAuthority_DELETED {
			out = append(out, console.Action{ID: "labels", Label: "Edit labels", Fields: []console.Field{labelsField(ca.GetLabels(), "")}})
		}
		if resp, err := p.api().ListCertificateRevocationLists(ctx, &privatecapb.ListCertificateRevocationListsRequest{Parent: ca.GetName()}); err == nil &&
			len(resp.GetCertificateRevocationLists()) > 0 && ca.GetState() != privatecapb.CertificateAuthority_DELETED {
			crl := resp.GetCertificateRevocationLists()[0]
			out = append(out, console.Action{ID: "crllabels", Label: "Edit CRL labels", Fields: []console.Field{
				{Name: "crl", Label: "Revocation list", Type: "text", Required: true, Default: crl.GetName()[strings.LastIndex(crl.GetName(), "/")+1:], Pattern: privateCAIDPattern},
				labelsField(crl.GetLabels(), "")}})
		}
		return out
	case len(path) == 3 && path[1] == "certificates":
		c, err := p.api().GetCertificate(ctx, &privatecapb.GetCertificateRequest{Name: path[0] + "/certificates/" + path[2]})
		if err != nil {
			return nil
		}
		out := []console.Action{}
		if c.GetRevocationDetails() == nil {
			// Google's Revoke ends in Confirm (revoking-certificates, read
			// 2026-11-22): the dialog is the confirmation.
			out = append(out, console.Action{ID: "revoke", Label: "Revoke", Destructive: true, Submit: "Confirm", ConfirmStyle: console.DeleteConfirmClick,
				Fields: []console.Field{
					{Name: "reason", Label: "Reason", Type: "select", Default: "REVOCATION_REASON_UNSPECIFIED", Options: privateCARevocationReasons(),
						Help: "A revoked certificate cannot be restored. It is listed in the CA's next CRL, published within 15 minutes."}}})
		}
		return append(out, console.Action{ID: "labels", Label: "Edit labels", Fields: []console.Field{labelsField(c.GetLabels(), "")}})
	}
	return nil
}

// privateCACreateCAFields are Create CA's steps as Google names them
// (creating-root-ca, create-subordinate-ca, read 2026-11-22): Select CA
// type, Configure CA subject name, Configure CA key size and algorithm,
// Configure CA artifacts and Add labels.
func privateCACreateCAFields() []console.Field {
	const typ, subj, key, art, lab = "Select CA type", "Configure CA subject name", "Configure CA key size and algorithm", "Configure CA artifacts", "Add labels"
	return []console.Field{
		{Name: "type", Label: "Type", Type: "radio", Required: true, Default: "SELF_SIGNED", Options: []string{"SELF_SIGNED", "SUBORDINATE"}, Section: typ,
			OptionLabels: map[string]string{"SELF_SIGNED": "Root CA", "SUBORDINATE": "Subordinate CA"}},
		{Name: "issuerCa", Label: "Issuing CA", Type: "text", ShowWhen: "type=SUBORDINATE", Section: typ,
			Help: "Root CA is in Google Cloud: the full name of an enabled CA of this instance, projects/…/caPools/…/certificateAuthorities/…. Empty: an external issuer signs the CSR, and the CA awaits activation."},
		{Name: "lifetimeDays", Label: "Valid for (days)", Type: "text", Required: true, Default: "3650", Pattern: `^[0-9]{1,5}$`, Section: typ},
		{Name: "initialState", Label: "Initialized state", Type: "radio", Default: "STAGED", Options: []string{"STAGED", "ENABLED"}, Section: typ,
			OptionLabels: map[string]string{"STAGED": "Staged", "ENABLED": "Enabled"},
			Help:         "A root CA starts Staged unless Enabled is chosen; a staged CA issues nothing until it is enabled."},
		{Name: "certificateAuthorityId", Label: "CA ID", Type: "text", Required: true, Pattern: privateCAIDPattern, Section: subj,
			Help: "The CA's resource ID in the pool, fixed at creation."},
		{Name: "organization", Label: "Organization (O)", Type: "text", Section: subj},
		{Name: "organizationalUnit", Label: "Organization unit (OU)", Type: "text", Section: subj},
		{Name: "countryCode", Label: "Country name (C)", Type: "text", Pattern: `^([A-Za-z]{2})?$`, Section: subj, Help: "A two-letter country code."},
		{Name: "province", Label: "State or province name", Type: "text", Section: subj},
		{Name: "locality", Label: "Locality name", Type: "text", Section: subj},
		{Name: "commonName", Label: "CA common name (CN)", Type: "text", Section: subj, Help: "Required unless the subject is an RDN sequence."},
		{Name: "rdnSequence", Label: "Subject RDN sequence", Type: "textarea", Help: privateCARDNHelp, Section: subj},
		{Name: "algorithm", Label: "Key algorithm", Type: "select", Required: true, Default: "RSA_PKCS1_4096_SHA256", Options: privateCAAlgorithms(), Section: key,
			Help: "A key Google manages for the CA. Ignored when a customer-managed key is given below."},
		{Name: "kmsKeyVersion", Label: "Customer-managed key", Type: "text", Section: key,
			Help: "Optional: the Cloud KMS key version that holds the CA's key, projects/…/locations/…/keyRings/…/cryptoKeys/…/cryptoKeyVersions/…, an enabled ASYMMETRIC_SIGN version in the pool's region."},
		{Name: "maxIssuerPathLength", Label: "Path length", Type: "text", Pattern: `^[0-9]{0,2}$`, Section: key,
			Help: "Optional: how many subordinate CAs may sit below this one."},
		{Name: "gcsBucket", Label: "Cloud Storage bucket", Type: "text", Browse: "storage", Section: art,
			Help: "Optional: where the CA certificate and CRLs are published. Empty: a Google-managed bucket in the CA's location."},
		withSection(labelsField(nil, ""), lab),
	}
}

func privateCAActivateAction() console.Action {
	return console.Action{ID: "activate", Label: "Activate", Fields: []console.Field{
		{Name: "pemCaCertificate", Label: "Signed CA certificate (PEM)", Type: "textarea", Required: true,
			Help: "The certificate the external issuer signed from this CA's CSR."},
		{Name: "pemIssuerChain", Label: "Issuer chain (PEM)", Type: "textarea", Required: true,
			Help: "The issuer's certificates, up to and including its root."},
	}}
}

func (p privateCAProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	api := p.api()
	if api == nil {
		return errors.New(privateCANotStarted)
	}
	pool, err := privateCAPool(project, path)
	if err != nil {
		return err
	}
	switch {
	case len(path) == 1:
		return p.poolAction(ctx, api, pool, action, values)
	case len(path) == 3 && path[1] == "certificateAuthorities":
		return p.caAction(ctx, api, pool+"/certificateAuthorities/"+path[2], action, values)
	case len(path) == 3 && path[1] == "certificates":
		return p.certAction(ctx, api, pool+"/certificates/"+path[2], action, values)
	}
	return fmt.Errorf("unknown action %q", action)
}

func (p privateCAProvider) poolAction(ctx context.Context, api *privateca.API, pool, action string, values map[string]string) error {
	switch action {
	case "createca":
		return privateCACreateCA(ctx, api, pool, values)
	case "requestcsr", "requestconfig":
		life, err := privateCADays("validity", values["lifetimeDays"])
		if err != nil {
			return err
		}
		notBefore, err := privateCAParseTime("requested not-before time", values["requestedNotBefore"])
		if err != nil {
			return err
		}
		mode := privatecapb.SubjectRequestMode_SUBJECT_REQUEST_MODE_UNSPECIFIED
		if m := strings.TrimSpace(values["subjectMode"]); m != "" {
			v, ok := privatecapb.SubjectRequestMode_value[m]
			if !ok {
				return fmt.Errorf("subject mode: %q is not one of %s", m, strings.Join(privateCASubjectModes(), ", "))
			}
			mode = privatecapb.SubjectRequestMode(v)
		}
		cert := &privatecapb.Certificate{Lifetime: life, CertificateTemplate: strings.TrimSpace(values["certificateTemplate"]),
			SubjectMode: mode, RequestedNotBeforeTime: notBefore}
		if action == "requestcsr" {
			cert.CertificateConfig = &privatecapb.Certificate_PemCsr{PemCsr: strings.TrimSpace(values["pemCsr"]) + "\n"}
		} else {
			var dns []string
			for _, d := range strings.Split(values["dnsNames"], ",") {
				if d = strings.TrimSpace(d); d != "" {
					dns = append(dns, d)
				}
			}
			eku := &privatecapb.KeyUsage_ExtendedKeyUsageOptions{}
			for _, u := range strings.Split(values["usage"], ",") {
				switch u {
				case "server_auth":
					eku.ServerAuth = true
				case "client_auth":
					eku.ClientAuth = true
				}
			}
			rdn, err := privateCAParseRDN(values["rdnSequence"])
			if err != nil {
				return err
			}
			custom, err := privateCAParseCustomSANs(values["customSans"])
			if err != nil {
				return err
			}
			cert.CertificateConfig = &privatecapb.Certificate_Config{Config: &privatecapb.CertificateConfig{
				SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{
					Subject:        &privatecapb.Subject{CommonName: strings.TrimSpace(values["commonName"]), RdnSequence: rdn},
					SubjectAltName: &privatecapb.SubjectAltNames{DnsNames: dns, CustomSans: custom}},
				PublicKey: &privatecapb.PublicKey{Format: privatecapb.PublicKey_PEM, Key: []byte(strings.TrimSpace(values["publicKey"]) + "\n")},
				X509Config: &privatecapb.X509Parameters{
					CaOptions: &privatecapb.X509Parameters_CaOptions{IsCa: new(bool)},
					KeyUsage: &privatecapb.KeyUsage{BaseKeyUsage: &privatecapb.KeyUsage_KeyUsageOptions{DigitalSignature: true, KeyEncipherment: true},
						ExtendedKeyUsage: eku}},
			}}
			if kid := strings.TrimSpace(values["subjectKeyId"]); kid != "" {
				cert.GetConfig().SubjectKeyId = &privatecapb.CertificateConfig_KeyId{KeyId: kid}
			}
		}
		_, err = api.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: pool, CertificateId: strings.TrimSpace(values["certificateId"]),
			Certificate: cert, IssuingCertificateAuthorityId: strings.TrimSpace(values["issuer"])})
		return err
	case "labels":
		labels, err := console.ParseMap(values["labels"])
		if err != nil {
			return err
		}
		_, err = api.UpdateCaPool(ctx, &privatecapb.UpdateCaPoolRequest{CaPool: &privatecapb.CaPool{Name: pool, Labels: labels},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
		return err
	case "deletepool":
		_, err := api.DeleteCaPool(ctx, &privatecapb.DeleteCaPoolRequest{Name: pool})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

func privateCACreateCA(ctx context.Context, api *privateca.API, pool string, values map[string]string) error {
	alg, ok := privatecapb.CertificateAuthority_SignHashAlgorithm_value[values["algorithm"]]
	if !ok {
		return fmt.Errorf("key algorithm: %q is not one of the CA key algorithms", values["algorithm"])
	}
	life, err := privateCADays("validity", values["lifetimeDays"])
	if err != nil {
		return err
	}
	if life == nil {
		return fmt.Errorf("validity is required")
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return err
	}
	rdn, err := privateCAParseRDN(values["rdnSequence"])
	if err != nil {
		return err
	}
	if len(rdn) == 0 && strings.TrimSpace(values["commonName"]) == "" {
		return fmt.Errorf("common name is required, or a subject RDN sequence")
	}
	caOpts := &privatecapb.X509Parameters_CaOptions{IsCa: pcaPtr(true)}
	if v := strings.TrimSpace(values["maxIssuerPathLength"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("path length: %q is not a number", v)
		}
		caOpts.MaxIssuerPathLength = pcaPtr(int32(n))
	}
	keySpec := &privatecapb.CertificateAuthority_KeyVersionSpec{KeyVersion: &privatecapb.CertificateAuthority_KeyVersionSpec_Algorithm{
		Algorithm: privatecapb.CertificateAuthority_SignHashAlgorithm(alg)}}
	if v := strings.TrimSpace(values["kmsKeyVersion"]); v != "" {
		keySpec.KeyVersion = &privatecapb.CertificateAuthority_KeyVersionSpec_CloudKmsKeyVersion{CloudKmsKeyVersion: v}
	}
	ca := &privatecapb.CertificateAuthority{
		Lifetime: life, Labels: labels, KeySpec: keySpec, GcsBucket: strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(values["gcsBucket"]), "gs://"), "/"),
		Config: &privatecapb.CertificateConfig{
			SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{Subject: &privatecapb.Subject{
				CommonName: strings.TrimSpace(values["commonName"]), Organization: strings.TrimSpace(values["organization"]),
				OrganizationalUnit: strings.TrimSpace(values["organizationalUnit"]), Province: strings.TrimSpace(values["province"]),
				Locality: strings.TrimSpace(values["locality"]), CountryCode: strings.TrimSpace(values["countryCode"]), RdnSequence: rdn}},
			X509Config: &privatecapb.X509Parameters{CaOptions: caOpts,
				KeyUsage: &privatecapb.KeyUsage{BaseKeyUsage: &privatecapb.KeyUsage_KeyUsageOptions{CertSign: true, CrlSign: true}}},
		},
	}
	issuer := strings.TrimSpace(values["issuerCa"])
	switch values["type"] {
	case "", "SELF_SIGNED":
		ca.Type = privatecapb.CertificateAuthority_SELF_SIGNED
		if issuer != "" {
			return fmt.Errorf("a root CA has no issuing CA")
		}
	case "SUBORDINATE":
		ca.Type = privatecapb.CertificateAuthority_SUBORDINATE
	default:
		return fmt.Errorf("CA type: %q is not SELF_SIGNED or SUBORDINATE", values["type"])
	}
	op, err := api.CreateCertificateAuthority(ctx, &privatecapb.CreateCertificateAuthorityRequest{Parent: pool,
		CertificateAuthorityId: strings.TrimSpace(values["certificateAuthorityId"]), CertificateAuthority: ca})
	if err != nil {
		return err
	}
	if ca.Type != privatecapb.CertificateAuthority_SUBORDINATE || issuer == "" {
		return privateCAInitialState(ctx, api, pool+"/certificateAuthorities/"+strings.TrimSpace(values["certificateAuthorityId"]), values)
	}
	// Signed by a CA of this service, as gcloud privateca subordinates
	// create --issuer-pool does it: the issuer's pool signs the CSR, and
	// the certificate activates the new CA.
	created := &privatecapb.CertificateAuthority{}
	if err := op.GetResponse().UnmarshalTo(created); err != nil {
		return err
	}
	csr, err := api.FetchCertificateAuthorityCsr(ctx, &privatecapb.FetchCertificateAuthorityCsrRequest{Name: created.GetName()})
	if err != nil {
		return err
	}
	issuerPool, issuerID, ok := strings.Cut(issuer, "/certificateAuthorities/")
	if !ok {
		return fmt.Errorf("issuing CA: %q is not projects/…/caPools/…/certificateAuthorities/…", issuer)
	}
	signed, err := api.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: issuerPool, IssuingCertificateAuthorityId: issuerID,
		CertificateId: "subordinate-" + strings.TrimSpace(values["certificateAuthorityId"]) + "-" + strconv.FormatInt(time.Now().Unix(), 36),
		Certificate:   &privatecapb.Certificate{Lifetime: life, CertificateConfig: &privatecapb.Certificate_PemCsr{PemCsr: csr.GetPemCsr()}}})
	if err != nil {
		return fmt.Errorf("the issuer did not sign the subordinate's CSR (the CA stays AWAITING_USER_ACTIVATION): %w", err)
	}
	if _, err = api.ActivateCertificateAuthority(ctx, &privatecapb.ActivateCertificateAuthorityRequest{Name: created.GetName(), PemCaCertificate: signed.GetPemCertificate(),
		SubordinateConfig: &privatecapb.SubordinateConfig{SubordinateConfig: &privatecapb.SubordinateConfig_CertificateAuthority{CertificateAuthority: issuer}}}); err != nil {
		return err
	}
	return privateCAInitialState(ctx, api, created.GetName(), values)
}

// privateCAInitialState applies Create CA's Initialized state: Enabled
// enables the CA the create left STAGED.
func privateCAInitialState(ctx context.Context, api *privateca.API, name string, values map[string]string) error {
	if values["initialState"] != "ENABLED" {
		return nil
	}
	ca, err := api.GetCertificateAuthority(ctx, &privatecapb.GetCertificateAuthorityRequest{Name: name})
	if err != nil || ca.GetState() != privatecapb.CertificateAuthority_STAGED {
		return err
	}
	_, err = api.EnableCertificateAuthority(ctx, &privatecapb.EnableCertificateAuthorityRequest{Name: name})
	return err
}

// pcaPtr is a pointer to v, for the optional fields of the API's messages.
func pcaPtr[T any](v T) *T { return &v }

func (p privateCAProvider) caAction(ctx context.Context, api *privateca.API, name, action string, values map[string]string) error {
	var err error
	switch action {
	case "enable":
		_, err = api.EnableCertificateAuthority(ctx, &privatecapb.EnableCertificateAuthorityRequest{Name: name})
	case "disable":
		_, err = api.DisableCertificateAuthority(ctx, &privatecapb.DisableCertificateAuthorityRequest{Name: name})
	case "delete":
		_, err = api.DeleteCertificateAuthority(ctx, &privatecapb.DeleteCertificateAuthorityRequest{Name: name,
			SkipGracePeriod: values["skipGracePeriod"] == "true", IgnoreActiveCertificates: values["ignoreActiveCertificates"] == "true"})
	case "undelete":
		_, err = api.UndeleteCertificateAuthority(ctx, &privatecapb.UndeleteCertificateAuthorityRequest{Name: name})
	case "activate":
		_, err = api.ActivateCertificateAuthority(ctx, &privatecapb.ActivateCertificateAuthorityRequest{Name: name,
			PemCaCertificate: strings.TrimSpace(values["pemCaCertificate"]) + "\n",
			SubordinateConfig: &privatecapb.SubordinateConfig{SubordinateConfig: &privatecapb.SubordinateConfig_PemIssuerChain{
				PemIssuerChain: &privatecapb.SubordinateConfig_SubordinateConfigChain{PemCertificates: splitPEMCerts(values["pemIssuerChain"])}}}})
	case "crllabels":
		var labels map[string]string
		if labels, err = console.ParseMap(values["labels"]); err != nil {
			return err
		}
		_, err = api.UpdateCertificateRevocationList(ctx, &privatecapb.UpdateCertificateRevocationListRequest{
			CertificateRevocationList: &privatecapb.CertificateRevocationList{Name: name + "/certificateRevocationLists/" + strings.TrimSpace(values["crl"]), Labels: labels},
			UpdateMask:                &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	case "labels":
		var labels map[string]string
		if labels, err = console.ParseMap(values["labels"]); err != nil {
			return err
		}
		_, err = api.UpdateCertificateAuthority(ctx, &privatecapb.UpdateCertificateAuthorityRequest{
			CertificateAuthority: &privatecapb.CertificateAuthority{Name: name, Labels: labels},
			UpdateMask:           &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	return err
}

// splitPEMCerts splits concatenated PEM certificates.
func splitPEMCerts(raw string) []string {
	var out []string
	for _, part := range strings.SplitAfter(strings.TrimSpace(raw), "-----END CERTIFICATE-----") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part+"\n")
		}
	}
	return out
}

func (p privateCAProvider) certAction(ctx context.Context, api *privateca.API, name, action string, values map[string]string) error {
	switch action {
	case "revoke":
		reason, ok := privatecapb.RevocationReason_value[values["reason"]]
		if !ok {
			return fmt.Errorf("reason: %q is not a revocation reason", values["reason"])
		}
		_, err := api.RevokeCertificate(ctx, &privatecapb.RevokeCertificateRequest{Name: name, Reason: privatecapb.RevocationReason(reason)})
		return err
	case "labels":
		labels, err := console.ParseMap(values["labels"])
		if err != nil {
			return err
		}
		_, err = api.UpdateCertificate(ctx, &privatecapb.UpdateCertificateRequest{Certificate: &privatecapb.Certificate{Name: name, Labels: labels},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// The Permissions tab on a CA pool's page: the policy the pool stores,
// read and written through the IAMPolicy mixin the SDK's
// GetIamPolicy/SetIamPolicy reach. It is stored, never enforced (ADR-0006).

func (privateCAProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	return &console.PolicyTarget{Link: compatibilityDoc + "certificate-authority-service--googlecloudsecurityprivatecav1",
		RoleHelp: roleHelp("Certificate Authority Service", "roles/privateca.certificateRequester"), Conditions: true}
}

func (p privateCAProvider) iam(project string, path []string) (iamServer, string, error) {
	s := p.srv()
	if s == nil {
		return nil, "", errors.New(privateCANotStarted)
	}
	if len(path) != 1 {
		return nil, "", fmt.Errorf("no IAM policy at %s", strings.Join(path, "/"))
	}
	pool, err := privateCAPool(project, path)
	if err != nil {
		return nil, "", err
	}
	return s.IAMPolicy(), pool, nil
}

func (p privateCAProvider) GetPolicy(ctx context.Context, project string, path []string) (console.Policy, error) {
	api, name, err := p.iam(project, path)
	if err != nil {
		return console.Policy{}, err
	}
	return getProtoPolicy(ctx, api, name)
}

func (p privateCAProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	api, name, err := p.iam(project, path)
	if err != nil {
		return err
	}
	return setProtoPolicy(ctx, api, name, pol)
}

func (p privateCAProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) ([]string, error) {
	api, name, err := p.iam(project, path)
	if err != nil {
		return nil, err
	}
	return testProtoPermissions(ctx, api, name, perms)
}

var (
	_ console.Creator      = privateCAProvider{}
	_ console.Driller      = privateCAProvider{}
	_ console.PathActor    = privateCAProvider{}
	_ console.PolicyEditor = privateCAProvider{}
)

// privateCAParseCustomSANs reads the Custom SANs field: one OID=text per
// line, each a custom SAN whose value is text as an ASN.1 UTF8String.
func privateCAParseCustomSANs(text string) ([]*privatecapb.X509Extension, error) {
	var out []*privatecapb.X509Extension
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		o, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("custom SAN %q is not OID=text", line)
		}
		id := &privatecapb.ObjectId{}
		for _, part := range strings.Split(strings.TrimSpace(o), ".") {
			n, err := strconv.ParseInt(part, 10, 32)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("custom SAN %q: %q is not an object identifier", line, o)
			}
			id.ObjectIdPath = append(id.ObjectIdPath, int32(n))
		}
		value, err := asn1.MarshalWithParams(strings.TrimSpace(v), "utf8")
		if err != nil {
			return nil, fmt.Errorf("custom SAN %q: %v", line, err)
		}
		out = append(out, &privatecapb.X509Extension{ObjectId: id, Value: value})
	}
	return out, nil
}
