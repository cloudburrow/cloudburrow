package main

import (
	"context"
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

func (privateCAProvider) CreateForm() (string, []console.Field) {
	return "Create CA pool", []console.Field{
		{Name: "caPoolId", Label: "Pool ID", Type: "text", Required: true, Pattern: privateCAIDPattern,
			Help: "1 to 63 letters, digits, hyphens and underscores. Fixed at creation."},
		{Name: "location", Label: "Region", Type: "select", Required: true, Default: "us-central1", Options: privateCALocations()},
		{Name: "tier", Label: "Tier", Type: "select", Required: true, Default: "ENTERPRISE", Options: []string{"ENTERPRISE", "DEVOPS"},
			Help: "Enterprise stores the certificates it issues, which can then be listed and revoked; DevOps does not store them."},
		{Name: "maximumLifetimeDays", Label: "Maximum certificate lifetime (days)", Type: "text", Pattern: `^[0-9]{0,5}$`,
			Help: "Optional: the issuance policy's maximum_lifetime. A longer request is shortened to it."},
		{Name: "allowCsr", Label: "Allow certificate requests from a CSR", Type: "checkbox", Default: "true"},
		{Name: "allowConfig", Label: "Allow certificate requests from a config", Type: "checkbox", Default: "true"},
		labelsField(nil, ""),
	}
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
	csr, cfg := values["allowCsr"] != "false", values["allowConfig"] != "false"
	if life != nil || !csr || !cfg {
		pool.IssuancePolicy = &privatecapb.CaPool_IssuancePolicy{MaximumLifetime: life}
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
	return []console.Property{
		{Label: "Allowed request types", Value: modes},
		{Label: "Maximum certificate lifetime", Value: life},
	}
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
					{Label: "Resource name", Value: name}, {Label: "Labels", Value: kmsOrDash(labelsText(pool.GetLabels()))}}},
				{Heading: "Issuance policy", Properties: privateCAPolicyText(pool.GetIssuancePolicy())}}},
			{ID: "cacerts", Label: "CA certificates", Kind: console.KindText, Text: anchors.String(),
				Note: "The pool's trust anchors, as FetchCaCerts answers them (gcloud privateca pools get-ca-certs)."},
		},
	}, nil
}

func firstDescription(ca *privatecapb.CertificateAuthority) *privatecapb.CertificateDescription {
	if ds := ca.GetCaCertificateDescriptions(); len(ds) > 0 {
		return ds[0]
	}
	return nil
}

// privateCASubject is a subject as an RFC 4514 string, its common name
// first.
func privateCASubject(s *privatecapb.Subject) string {
	if s == nil {
		return ""
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
		{Label: "Key algorithm", Value: ca.GetKeySpec().GetAlgorithm().String()},
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
	return console.Detail{
		Summary: []console.Property{
			{Label: "State", Value: ca.GetState().String()},
			{Label: "Created", Value: kmsTime(ca.GetCreateTime())},
		},
		Sections: []console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Certificate authority", Properties: props}}, Note: privateCANote},
			{ID: "pem", Label: "CA certificate chain", Kind: console.KindText, Text: kmsOrDash(strings.Join(ca.GetPemCaCertificates(), ""))},
		},
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
	return console.Detail{
		Summary: []console.Property{
			{Label: "Status", Value: status},
			{Label: "Created", Value: kmsTime(c.GetCreateTime())},
		},
		Sections: []console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Certificate", Properties: props}}},
			{ID: "pem", Label: "Certificate", Kind: console.KindText, Text: c.GetPemCertificate()},
			{ID: "chain", Label: "Certificate chain", Kind: console.KindText, Text: kmsOrDash(strings.Join(c.GetPemCertificateChain(), ""))},
		},
	}, nil
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
				{Name: "issuer", Label: "Issuing CA ID", Type: "text", Pattern: `^([A-Za-z0-9_\-]{1,63})?$`, Help: "Optional: empty is any enabled CA of the pool."},
			}},
			{ID: "requestconfig", Label: "Request certificate from a config", Fields: []console.Field{
				{Name: "certificateId", Label: "Certificate ID", Type: "text", Pattern: `^([A-Za-z0-9_\-]{1,63})?$`,
					Help: "Required in an Enterprise pool; in a DevOps pool, empty is generated."},
				{Name: "commonName", Label: "Common name", Type: "text"},
				{Name: "dnsNames", Label: "DNS names", Type: "text", Help: "Comma-separated subject alternative names."},
				{Name: "publicKey", Label: "Public key (PEM)", Type: "textarea", Required: true},
				{Name: "usage", Label: "Extended key usage", Type: "select", Default: "server_auth", Options: []string{"server_auth", "client_auth", "server_auth,client_auth"}},
				{Name: "lifetimeDays", Label: "Validity (days)", Type: "text", Default: "30", Pattern: `^[0-9]{1,5}$`},
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
			out = append(out, console.Action{ID: "enable", Label: "Enable"})
		}
		switch ca.GetState() {
		case privatecapb.CertificateAuthority_STAGED, privatecapb.CertificateAuthority_ENABLED:
			out = append(out, console.Action{ID: "disable", Label: "Disable",
				Confirm: "A disabled CA issues no certificates; the pool's other enabled CAs still do."})
		}
		if ca.GetState() == privatecapb.CertificateAuthority_AWAITING_USER_ACTIVATION {
			out = append(out, privateCAActivateAction())
		}
		switch ca.GetState() {
		case privatecapb.CertificateAuthority_DISABLED, privatecapb.CertificateAuthority_STAGED, privatecapb.CertificateAuthority_AWAITING_USER_ACTIVATION:
			out = append(out, console.Action{ID: "delete", Label: "Delete", Destructive: true, Fields: []console.Field{
				{Name: "skipGracePeriod", Label: "Delete immediately, without the 30-day grace period", Type: "checkbox"},
				{Name: "ignoreActiveCertificates", Label: "Delete even with active certificates", Type: "checkbox"}}})
		case privatecapb.CertificateAuthority_DELETED:
			out = append(out, console.Action{ID: "undelete", Label: "Undelete"})
		}
		if ca.GetState() != privatecapb.CertificateAuthority_DELETED {
			out = append(out, console.Action{ID: "labels", Label: "Edit labels", Fields: []console.Field{labelsField(ca.GetLabels(), "")}})
		}
		return out
	case len(path) == 3 && path[1] == "certificates":
		c, err := p.api().GetCertificate(ctx, &privatecapb.GetCertificateRequest{Name: path[0] + "/certificates/" + path[2]})
		if err != nil {
			return nil
		}
		out := []console.Action{}
		if c.GetRevocationDetails() == nil {
			out = append(out, console.Action{ID: "revoke", Label: "Revoke", Destructive: true,
				Confirm: "A revoked certificate cannot be restored.", Fields: []console.Field{
					{Name: "reason", Label: "Reason", Type: "select", Default: "REVOCATION_REASON_UNSPECIFIED", Options: privateCARevocationReasons()}}})
		}
		return append(out, console.Action{ID: "labels", Label: "Edit labels", Fields: []console.Field{labelsField(c.GetLabels(), "")}})
	}
	return nil
}

func privateCACreateCAFields() []console.Field {
	return []console.Field{
		{Name: "certificateAuthorityId", Label: "CA ID", Type: "text", Required: true, Pattern: privateCAIDPattern},
		{Name: "type", Label: "CA type", Type: "select", Required: true, Default: "SELF_SIGNED", Options: []string{"SELF_SIGNED", "SUBORDINATE"},
			Help: "SELF_SIGNED is a root CA. SUBORDINATE is signed by the CA named below, or, with none, awaits activation by a third-party issuer."},
		{Name: "issuerCa", Label: "Issuing CA (subordinate only)", Type: "text",
			Help: "Optional: the full name of an enabled CA of this instance, projects/…/caPools/…/certificateAuthorities/…. Empty: an external issuer signs the CSR."},
		{Name: "commonName", Label: "Common name", Type: "text", Required: true},
		{Name: "organization", Label: "Organization", Type: "text"},
		{Name: "countryCode", Label: "Country code", Type: "text", Pattern: `^([A-Za-z]{2})?$`},
		{Name: "algorithm", Label: "Key algorithm", Type: "select", Required: true, Default: "RSA_PKCS1_4096_SHA256", Options: privateCAAlgorithms()},
		{Name: "lifetimeDays", Label: "Validity (days)", Type: "text", Required: true, Default: "3650", Pattern: `^[0-9]{1,5}$`},
		{Name: "maxIssuerPathLength", Label: "Path length", Type: "text", Pattern: `^[0-9]{0,2}$`,
			Help: "Optional: how many subordinate CAs may sit below this one."},
		labelsField(nil, ""),
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
		cert := &privatecapb.Certificate{Lifetime: life}
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
			cert.CertificateConfig = &privatecapb.Certificate_Config{Config: &privatecapb.CertificateConfig{
				SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{
					Subject:        &privatecapb.Subject{CommonName: strings.TrimSpace(values["commonName"])},
					SubjectAltName: &privatecapb.SubjectAltNames{DnsNames: dns}},
				PublicKey: &privatecapb.PublicKey{Format: privatecapb.PublicKey_PEM, Key: []byte(strings.TrimSpace(values["publicKey"]) + "\n")},
				X509Config: &privatecapb.X509Parameters{
					CaOptions: &privatecapb.X509Parameters_CaOptions{IsCa: new(bool)},
					KeyUsage: &privatecapb.KeyUsage{BaseKeyUsage: &privatecapb.KeyUsage_KeyUsageOptions{DigitalSignature: true, KeyEncipherment: true},
						ExtendedKeyUsage: eku}},
			}}
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
	caOpts := &privatecapb.X509Parameters_CaOptions{IsCa: pcaPtr(true)}
	if v := strings.TrimSpace(values["maxIssuerPathLength"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("path length: %q is not a number", v)
		}
		caOpts.MaxIssuerPathLength = pcaPtr(int32(n))
	}
	ca := &privatecapb.CertificateAuthority{
		Lifetime: life, Labels: labels,
		KeySpec: &privatecapb.CertificateAuthority_KeyVersionSpec{KeyVersion: &privatecapb.CertificateAuthority_KeyVersionSpec_Algorithm{
			Algorithm: privatecapb.CertificateAuthority_SignHashAlgorithm(alg)}},
		Config: &privatecapb.CertificateConfig{
			SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{Subject: &privatecapb.Subject{
				CommonName: strings.TrimSpace(values["commonName"]), Organization: strings.TrimSpace(values["organization"]),
				CountryCode: strings.TrimSpace(values["countryCode"])}},
			X509Config: &privatecapb.X509Parameters{CaOptions: caOpts,
				KeyUsage: &privatecapb.KeyUsage{BaseKeyUsage: &privatecapb.KeyUsage_KeyUsageOptions{CertSign: true, CrlSign: true}}},
		},
	}
	switch values["type"] {
	case "", "SELF_SIGNED":
		ca.Type = privatecapb.CertificateAuthority_SELF_SIGNED
	case "SUBORDINATE":
		ca.Type = privatecapb.CertificateAuthority_SUBORDINATE
		if issuer := strings.TrimSpace(values["issuerCa"]); issuer != "" {
			ca.SubordinateConfig = &privatecapb.SubordinateConfig{SubordinateConfig: &privatecapb.SubordinateConfig_CertificateAuthority{CertificateAuthority: issuer}}
		}
	default:
		return fmt.Errorf("CA type: %q is not SELF_SIGNED or SUBORDINATE", values["type"])
	}
	_, err = api.CreateCertificateAuthority(ctx, &privatecapb.CreateCertificateAuthorityRequest{Parent: pool,
		CertificateAuthorityId: strings.TrimSpace(values["certificateAuthorityId"]), CertificateAuthority: ca})
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
