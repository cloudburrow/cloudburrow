package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Certificate Manager's pages (#2160), over certificatemanager.googleapis.com
// v1 (served on the Compute port), as Google's console has them
// (certificate-manager/docs/deploy-google-managed-dns-auth and
// .../deploy-google-managed-lb-auth, read 2026-11-22): Security ›
// Certificate Manager, its tabs Certificates (Add Certificate: Certificate
// name, Description, Location, Scope, Certificate type, Certificate
// Authority type, Domain Names, Authorization type, DNS authorizations, or
// a self-managed certificate's PEM), Certificate Maps (a map's entries,
// each a hostname or the primary entry with its certificates), DNS
// Authorizations (the CNAME record each asks for), Issuance Configs (a
// private CA pool's issuance) and Classic Certificates (#2153). The
// columns, and the map and issuance config forms, are UNVERIFIED (re-read
// 2026-11-22: the docs give gcloud only for them). A Google-managed
// certificate is issued by this instance's certificate authority
// (/cloudburrow/lb/ca.pem) once it is authorized, a private one by the
// issuance config's CA pool.

const ccmNote = "Certificates here are served by the load balancers on this machine: a certificate map attached to a target HTTPS or SSL proxy answers each TLS handshake with its entry's certificate for the client's server name. " +
	"Google-managed certificates are issued by this instance's certificate authority (/cloudburrow/lb/ca.pem on the Compute Engine API's port)."

// ccmScreen is what the Certificate Manager tabs share.
type ccmScreen struct{ computeScreen }

// ccmList is a collection's items in every location ("-"), sorted.
func (c ccmScreen) ccmList(ctx context.Context, project, coll string) ([]map[string]any, error) {
	var out []map[string]any
	token := ""
	for {
		u := "/v1/projects/" + project + "/locations/-/" + coll + "?pageSize=500"
		if token != "" {
			u += "&pageToken=" + url.QueryEscape(token)
		}
		m, err := c.v1(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, vmMaps(m, coll)...)
		if token = vpcStr(m, "nextPageToken"); token == "" {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return vpcStr(out[i], "name") < vpcStr(out[j], "name") })
	return out, nil
}

// ccmPath is a resource's [location, id].
func ccmPath(name string) []string {
	p := strings.Split(name, "/")
	if len(p) < 6 {
		return []string{"global", vpcLast(name)}
	}
	return []string{p[3], p[5]}
}

func ccmName(project string, path []string, coll string) string {
	return "projects/" + project + "/locations/" + path[0] + "/" + coll + "/" + path[1]
}

func ccmLocationText(l string) string {
	if l == "global" {
		return "Global"
	}
	return l
}

func ccmDelete(noun string, path []string) console.Action {
	return console.Action{ID: "delete", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "Delete " + noun + " " + path[1] + "?"}
}

// ccmCreate posts a resource and waits for nothing: the operation it
// answers is done at once here.
func (c ccmScreen) ccmCreate(ctx context.Context, project, location, coll, idParam, id string, body map[string]any) error {
	if id == "" {
		return errors.New("Name: give one")
	}
	_, err := c.v1(ctx, "POST", "/v1/projects/"+project+"/locations/"+location+"/"+coll+"?"+idParam+"="+url.QueryEscape(id), body)
	return err
}

func ccmLocationFields(section string) []console.Field {
	return []console.Field{
		{Name: "location", Label: "Location", Type: "radio", Default: "global", Options: []string{"global", "regional"}, Section: section,
			OptionLabels: map[string]string{"global": "Global", "regional": "Regional"}},
		{Name: "region", Label: "Region", Type: "select", Default: "us-central1", Options: computeRegionField("").Options, Section: section,
			VisibleWhen: map[string]string{"location": "regional"}},
	}
}

func ccmLocation(v map[string]string) string {
	if v["location"] == "regional" {
		return vpcOr(v["region"], "us-central1")
	}
	return "global"
}

// --- Certificates ------------------------------------------------------------

type ccmCertsProvider struct{ ccmScreen }

func (ccmCertsProvider) ID() string    { return "ccm-certificates" }
func (ccmCertsProvider) Title() string { return "Certificate Manager" }

func ccmCertType(o map[string]any) string {
	if len(vmMap(o, "managed")) > 0 {
		if vpcStr(vmMap(o, "managed"), "issuanceConfig") != "" {
			return "Google-managed (private)"
		}
		return "Google-managed"
	}
	return "Self-managed"
}

func ccmCertStatus(o map[string]any) string {
	if m := vmMap(o, "managed"); len(m) > 0 {
		return vpcTitle(strings.ToLower(vpcOr(vpcStr(m, "state"), "PROVISIONING")))
	}
	return "Active"
}

func (p ccmCertsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Location", "Scope", "Type", "Domain names", "Expires", "Status"}, NameColumn: "Name", Noun: "certificates", AlwaysStatus: true, Note: ccmNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Certificates are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.ccmList(ctx, project, "certificates")
	if err != nil {
		listing.Unavailable = err.Error()
		return listing, nil
	}
	for _, o := range l {
		path := ccmPath(vpcStr(o, "name"))
		st := ccmCertStatus(o)
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Opens: path, Target: path, Status: st,
			Fields: map[string]string{"Location": ccmLocationText(path[0]), "Scope": vpcTitle(strings.ToLower(strings.ReplaceAll(vpcOr(vpcStr(o, "scope"), "DEFAULT"), "_", " "))),
				"Type": ccmCertType(o), "Domain names": vpcDash(strings.Join(vpcStrs(o, "sanDnsnames"), ", ")), "Expires": vpcDash(vpcStr(o, "expireTime")), "Status": st},
			Actions: []console.Action{ccmDelete("certificate", path)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (ccmCertsProvider) CreateOnPage() bool   { return true }
func (ccmCertsProvider) CreateTitle() string  { return "Add Certificate" }
func (ccmCertsProvider) CreateLayout() string { return console.CreateLayoutPage }

func (ccmCertsProvider) CreateForm() (string, []console.Field) {
	managed := map[string]string{"type": "managed"}
	self := map[string]string{"type": "self"}
	fields := []console.Field{
		{Name: "name", Label: "Certificate name", Type: "text", Required: true, Pattern: computeNamePattern, Help: "A unique name for the certificate."},
		{Name: "description", Label: "Description", Type: "text", Help: "Optional."},
	}
	fields = append(fields, ccmLocationFields("")...)
	return "Create", append(fields,
		console.Field{Name: "scope", Label: "Scope", Type: "select", Default: "DEFAULT", Options: []string{"DEFAULT", "ALL_REGIONS", "EDGE_CACHE"},
			OptionLabels: map[string]string{"DEFAULT": "Default", "ALL_REGIONS": "All regions", "EDGE_CACHE": "Edge cache"}},
		console.Field{Name: "type", Label: "Certificate type", Type: "radio", Default: "managed", Options: []string{"managed", "self"},
			OptionLabels: map[string]string{"managed": "Create Google-managed certificate", "self": "Create self-managed certificate"}},
		console.Field{Name: "caType", Label: "Certificate Authority type", Type: "radio", Default: "public", Options: []string{"public", "private"},
			OptionLabels: map[string]string{"public": "Public", "private": "Private"}, VisibleWhen: managed},
		console.Field{Name: "domains", Label: "Domain Names", Type: "text", VisibleWhen: managed, Help: "A comma-separated list of domain names of the certificate."},
		console.Field{Name: "authorization", Label: "Authorization type", Type: "radio", Default: "lb", Options: []string{"lb", "dns"},
			OptionLabels: map[string]string{"lb": "Load balancer authorization", "dns": "DNS authorization"}, ShowWhen: "caType=public"},
		console.Field{Name: "dnsAuthorizations", Label: "DNS authorizations", Type: "text", VisibleWhen: map[string]string{"authorization": "dns"},
			Help: "The DNS authorizations' names, one for each domain, comma separated."},
		console.Field{Name: "issuanceConfig", Label: "Certificate issuance config", Type: "text", VisibleWhen: map[string]string{"caType": "private"},
			Help: "The name of a certificate issuance config in the certificate's location."},
		console.Field{Name: "certificate", Label: "Certificate", Type: "textarea", VisibleWhen: self, Help: "PEM: the certificate, then its chain."},
		console.Field{Name: "privateKey", Label: "Private key", Type: "textarea", VisibleWhen: self, Help: "PEM: the certificate's private key."},
	)
}

func (p ccmCertsProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name := strings.TrimSpace(v["name"])
	loc := ccmLocation(v)
	body := map[string]any{"scope": vpcOr(v["scope"], "DEFAULT")}
	if d := strings.TrimSpace(v["description"]); d != "" {
		body["description"] = d
	}
	if v["type"] == "self" {
		body["selfManaged"] = map[string]any{"pemCertificate": v["certificate"], "pemPrivateKey": v["privateKey"]}
	} else {
		m := map[string]any{"domains": vpcList(v["domains"])}
		switch {
		case v["caType"] == "private":
			m["issuanceConfig"] = "projects/" + project + "/locations/" + loc + "/certificateIssuanceConfigs/" + strings.TrimSpace(v["issuanceConfig"])
		case v["authorization"] == "dns":
			var auths []any
			for _, a := range vpcList(v["dnsAuthorizations"]) {
				auths = append(auths, "projects/"+project+"/locations/"+loc+"/dnsAuthorizations/"+fmt.Sprint(a))
			}
			m["dnsAuthorizations"] = auths
		}
		body["managed"] = m
	}
	if err := p.ccmCreate(ctx, project, loc, "certificates", "certificateId", name, body); err != nil {
		return "", err
	}
	return name, nil
}

func (p ccmCertsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.Detail{Unavailable: "a certificate is addressed by its location and name"}, nil
	}
	o, err := p.v1(ctx, "GET", "/v1/"+ccmName(project, path, "certificates"), nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	props := []console.Property{{Label: "Name", Value: path[1]}, {Label: "Description", Value: vpcDash(vpcStr(o, "description"))},
		{Label: "Location", Value: ccmLocationText(path[0])}, {Label: "Scope", Value: vpcOr(vpcStr(o, "scope"), "DEFAULT")},
		{Label: "Type", Value: ccmCertType(o)}, {Label: "Status", Value: ccmCertStatus(o)},
		{Label: "Domain names", Value: vpcDash(strings.Join(vpcStrs(o, "sanDnsnames"), ", "))},
		{Label: "Expires", Value: vpcDash(vpcStr(o, "expireTime"))}}
	auth := console.Listing{Columns: []string{"State", "Details"}, NameColumn: "Domain", Noun: "domain authorizations", Items: []console.Resource{}}
	m := vmMap(o, "managed")
	for _, a := range vmMaps(m, "authorizationAttemptInfo") {
		auth.Items = append(auth.Items, console.Resource{Name: vpcStr(a, "domain"), Fields: map[string]string{"State": vpcTitle(strings.ToLower(vpcStr(a, "state"))),
			"Details": vpcDash(vpcOr(vpcStr(a, "details"), vpcStr(a, "failureReason")))}})
	}
	if len(m) > 0 {
		props = append(props, console.Property{Label: "DNS authorizations", Value: vpcDash(strings.Join(lastAll(vpcStrs(m, "dnsAuthorizations")), ", "))},
			console.Property{Label: "Certificate issuance config", Value: vpcDash(vpcLast(vpcStr(m, "issuanceConfig")))})
	}
	auth.Total = len(auth.Items)
	secs := []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}}}
	if len(m) > 0 {
		secs = append(secs, console.Section{ID: "authorization", Label: "Authorization", Listing: auth})
	}
	if pem := vpcStr(o, "pemCertificate"); pem != "" {
		secs = append(secs, console.Section{ID: "pem", Label: "Certificate", Kind: console.KindText, Text: pem})
	}
	return console.Detail{Title: path[1], Heading: "Certificate details", Sections: secs}, nil
}

func lastAll(l []string) []string {
	out := make([]string, 0, len(l))
	for _, s := range l {
		out = append(out, vpcLast(s))
	}
	return out
}

func (p ccmCertsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil || len(path) != 2 {
		return nil
	}
	a := ccmDelete("certificate", path)
	a.Leaves = true
	return []console.Action{a}
}

func (p ccmCertsProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.v1(ctx, "DELETE", "/v1/"+ccmName(project, path, "certificates"), nil)
	return err
}

// --- Certificate maps ----------------------------------------------------------

type ccmMapsProvider struct{ ccmScreen }

func (ccmMapsProvider) ID() string    { return "ccm-maps" }
func (ccmMapsProvider) Title() string { return "Certificate Manager" }

func ccmTargets(o map[string]any) []string {
	var out []string
	for _, t := range vmMaps(o, "gclbTargets") {
		px := vpcOr(vpcStr(t, "targetHttpsProxy"), vpcStr(t, "targetSslProxy"))
		if px != "" {
			out = append(out, vpcLast(px))
		}
	}
	return out
}

func (p ccmMapsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Location", "Description", "Entries", "Targets", "Created"}, NameColumn: "Name", Noun: "certificate maps", Note: ccmNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Certificate maps are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.ccmList(ctx, project, "certificateMaps")
	if err != nil {
		listing.Unavailable = err.Error()
		return listing, nil
	}
	for _, o := range l {
		path := ccmPath(vpcStr(o, "name"))
		n := len(p.entries(ctx, project, path))
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Opens: path, Target: path,
			Fields: map[string]string{"Location": ccmLocationText(path[0]), "Description": vpcDash(vpcStr(o, "description")), "Entries": strconv.Itoa(n),
				"Targets": vpcDash(strings.Join(ccmTargets(o), ", ")), "Created": vpcDash(vpcStr(o, "createTime"))},
			Actions: []console.Action{ccmDelete("certificate map", path)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (p ccmMapsProvider) entries(ctx context.Context, project string, path []string) []map[string]any {
	m, err := p.v1(ctx, "GET", "/v1/"+ccmName(project, path, "certificateMaps")+"/certificateMapEntries?pageSize=500", nil)
	if err != nil {
		return nil
	}
	return vmMaps(m, "certificateMapEntries")
}

func (ccmMapsProvider) CreateOnPage() bool   { return true }
func (ccmMapsProvider) CreateTitle() string  { return "Create certificate map" }
func (ccmMapsProvider) CreateLayout() string { return console.CreateLayoutPage }

func (ccmMapsProvider) CreateForm() (string, []console.Field) {
	return "Create", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern, Help: computeNameHelp},
		{Name: "description", Label: "Description", Type: "text", Help: "Optional."},
	}
}

func (p ccmMapsProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name := strings.TrimSpace(v["name"])
	body := map[string]any{}
	if d := strings.TrimSpace(v["description"]); d != "" {
		body["description"] = d
	}
	if err := p.ccmCreate(ctx, project, "global", "certificateMaps", "certificateMapId", name, body); err != nil {
		return "", err
	}
	return name, nil
}

func ccmAddEntryAction() console.Action {
	return console.Action{ID: "addentry", Label: "Add certificate map entry", Title: "Add certificate map entry", Submit: "Create", Fields: []console.Field{
		{Name: "entry", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern},
		{Name: "matchType", Label: "Match", Type: "radio", Default: "hostname", Options: []string{"hostname", "primary"},
			OptionLabels: map[string]string{"hostname": "Hostname", "primary": "Primary entry"},
			Help:         "The primary entry serves a handshake whose server name matches no other entry, or that gives none."},
		{Name: "hostname", Label: "Hostname", Type: "text", VisibleWhen: map[string]string{"matchType": "hostname"},
			Help: "A host name, such as www.example.com, or a wildcard such as *.example.com."},
		{Name: "certificates", Label: "Certificates", Type: "text", Required: true,
			Help: "Certificates' names (global, Default scope), comma separated; the first whose key type the client supports is served."},
	}}
}

func (p ccmMapsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if p.api() == nil {
		return console.Detail{Unavailable: computeNotStarted}, nil
	}
	if len(path) != 2 {
		return console.Detail{Unavailable: "a certificate map is addressed by its location and name"}, nil
	}
	o, err := p.v1(ctx, "GET", "/v1/"+ccmName(project, path, "certificateMaps"), nil)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	props := []console.Property{{Label: "Name", Value: path[1]}, {Label: "Description", Value: vpcDash(vpcStr(o, "description"))},
		{Label: "Location", Value: ccmLocationText(path[0])}, {Label: "Targets", Value: vpcDash(strings.Join(ccmTargets(o), ", "))},
		{Label: "Created", Value: vpcDash(vpcStr(o, "createTime"))}}
	ents := console.Listing{Columns: []string{"Hostname", "Certificates", "State"}, NameColumn: "Entry", Noun: "certificate map entries", Items: []console.Resource{}}
	for _, e := range p.entries(ctx, project, path) {
		id := vpcLast(vpcStr(e, "name"))
		host := vpcStr(e, "hostname")
		if vpcStr(e, "matcher") == "PRIMARY" {
			host = "Primary entry"
		}
		ents.Items = append(ents.Items, console.Resource{Name: id, Target: []string{path[0], path[1], "entry", id},
			Fields: map[string]string{"Hostname": vpcDash(host), "Certificates": vpcDash(strings.Join(lastAll(vpcStrs(e, "certificates")), ", ")),
				"State": vpcTitle(strings.ToLower(vpcOr(vpcStr(e, "state"), "ACTIVE")))},
			Actions: []console.Action{{ID: "deleteentry", Label: "Delete", Destructive: true, ConfirmStyle: console.DeleteConfirmClick, Confirm: "Delete certificate map entry " + id + "?"}}})
	}
	ents.Total = len(ents.Items)
	return console.Detail{Title: path[1], Heading: "Certificate map details",
		Sections: []console.Section{{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: props}}},
			{ID: "entries", Label: "Certificate map entries", Listing: ents, Actions: []console.Action{ccmAddEntryAction()}}}}, nil
}

func (p ccmMapsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if p.api() == nil {
		return nil
	}
	switch {
	case len(path) == 2:
		a := ccmDelete("certificate map", path)
		a.Leaves = true
		return []console.Action{ccmAddEntryAction(), a}
	case len(path) == 4 && path[2] == "entry":
		return []console.Action{{ID: "deleteentry", Label: "Delete", Destructive: true}}
	}
	return nil
}

func (p ccmMapsProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) < 2 {
		return fmt.Errorf("%s is not available here", action)
	}
	base := ccmName(project, path, "certificateMaps")
	switch action {
	case "delete":
		_, err := p.v1(ctx, "DELETE", "/v1/"+base, nil)
		return err
	case "addentry":
		var certs []any
		for _, c := range vpcList(v["certificates"]) {
			certs = append(certs, "projects/"+project+"/locations/global/certificates/"+fmt.Sprint(c))
		}
		body := map[string]any{"certificates": certs}
		if v["matchType"] == "primary" {
			body["matcher"] = "PRIMARY"
		} else {
			h := strings.TrimSpace(v["hostname"])
			if h == "" {
				return errors.New("Hostname: give one, or choose Primary entry")
			}
			body["hostname"] = h
		}
		id := strings.TrimSpace(v["entry"])
		_, err := p.v1(ctx, "POST", "/v1/"+base+"/certificateMapEntries?certificateMapEntryId="+url.QueryEscape(id), body)
		return err
	case "deleteentry":
		if len(path) != 4 {
			return errors.New("Delete applies to an entry")
		}
		_, err := p.v1(ctx, "DELETE", "/v1/"+base+"/certificateMapEntries/"+path[3], nil)
		return err
	}
	return fmt.Errorf("%s is not available here", action)
}

// --- DNS authorizations -----------------------------------------------------

type ccmDNSAuthProvider struct{ ccmScreen }

func (ccmDNSAuthProvider) ID() string    { return "ccm-dnsauth" }
func (ccmDNSAuthProvider) Title() string { return "Certificate Manager" }

func (p ccmDNSAuthProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Domain", "Location", "Type", "DNS record name", "DNS record type", "DNS record data"}, NameColumn: "Name", Noun: "DNS authorizations",
		Note: "Each DNS authorization asks for the CNAME record shown in the domain's zone; Cloud DNS is not consulted here, so a certificate authorized by it is issued once it is created."}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "DNS authorizations are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.ccmList(ctx, project, "dnsAuthorizations")
	if err != nil {
		listing.Unavailable = err.Error()
		return listing, nil
	}
	for _, o := range l {
		path := ccmPath(vpcStr(o, "name"))
		rec := vmMap(o, "dnsResourceRecord")
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Target: path,
			Fields: map[string]string{"Domain": vpcStr(o, "domain"), "Location": ccmLocationText(path[0]), "Type": vpcTitle(strings.ToLower(strings.ReplaceAll(vpcOr(vpcStr(o, "type"), "FIXED_RECORD"), "_", " "))),
				"DNS record name": vpcDash(vpcStr(rec, "name")), "DNS record type": vpcDash(vpcStr(rec, "type")), "DNS record data": vpcDash(vpcStr(rec, "data"))},
			Actions: []console.Action{ccmDelete("DNS authorization", path)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (ccmDNSAuthProvider) CreateOnPage() bool   { return true }
func (ccmDNSAuthProvider) CreateTitle() string  { return "Create DNS authorization" }
func (ccmDNSAuthProvider) CreateLayout() string { return console.CreateLayoutPage }

func (ccmDNSAuthProvider) CreateForm() (string, []console.Field) {
	fields := []console.Field{
		{Name: "name", Label: "DNS authorization name", Type: "text", Required: true, Pattern: computeNamePattern},
		{Name: "description", Label: "Description", Type: "text", Help: "Optional."},
		{Name: "domain", Label: "Domain name", Type: "text", Required: true, Help: "The domain the certificates it authorizes are for, such as example.com; it covers its subdomains' wildcard too."},
		{Name: "type", Label: "Type", Type: "select", Default: "FIXED_RECORD", Options: []string{"FIXED_RECORD", "PER_PROJECT_RECORD"},
			OptionLabels: map[string]string{"FIXED_RECORD": "Fixed record", "PER_PROJECT_RECORD": "Per project record"}},
	}
	return "Create DNS authorization", append(fields, ccmLocationFields("")...)
}

func (p ccmDNSAuthProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name := strings.TrimSpace(v["name"])
	body := map[string]any{"domain": strings.TrimSpace(v["domain"]), "type": vpcOr(v["type"], "FIXED_RECORD")}
	if d := strings.TrimSpace(v["description"]); d != "" {
		body["description"] = d
	}
	if err := p.ccmCreate(ctx, project, ccmLocation(v), "dnsAuthorizations", "dnsAuthorizationId", name, body); err != nil {
		return "", err
	}
	return name, nil
}

func (p ccmDNSAuthProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.v1(ctx, "DELETE", "/v1/"+ccmName(project, path, "dnsAuthorizations"), nil)
	return err
}

// --- Issuance configs -----------------------------------------------------

type ccmIssuanceProvider struct{ ccmScreen }

func (ccmIssuanceProvider) ID() string    { return "ccm-issuance" }
func (ccmIssuanceProvider) Title() string { return "Certificate Manager" }

func (p ccmIssuanceProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Location", "CA pool", "Lifetime", "Rotation window", "Key algorithm"}, NameColumn: "Name", Noun: "certificate issuance configs", Note: ccmNote}
	if p.api() == nil {
		return listing, errors.New(computeNotStarted)
	}
	if project == "" {
		listing.Prompt = "Certificate issuance configs are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	l, err := p.ccmList(ctx, project, "certificateIssuanceConfigs")
	if err != nil {
		listing.Unavailable = err.Error()
		return listing, nil
	}
	for _, o := range l {
		path := ccmPath(vpcStr(o, "name"))
		pool := vpcStr(vmMap(vmMap(o, "certificateAuthorityConfig"), "certificateAuthorityServiceConfig"), "caPool")
		listing.Items = append(listing.Items, console.Resource{Name: path[1], Target: path,
			Fields: map[string]string{"Location": ccmLocationText(path[0]), "CA pool": vpcDash(pool), "Lifetime": vpcDash(vpcStr(o, "lifetime")),
				"Rotation window": vpcDash(num2s(o, "rotationWindowPercentage")) + map[bool]string{true: "%", false: ""}[num2s(o, "rotationWindowPercentage") != ""],
				"Key algorithm":   vpcDash(vpcStr(o, "keyAlgorithm"))},
			Actions: []console.Action{ccmDelete("certificate issuance config", path)}})
	}
	listing.Total = len(listing.Items)
	return listing, nil
}

func (ccmIssuanceProvider) CreateOnPage() bool   { return true }
func (ccmIssuanceProvider) CreateTitle() string  { return "Create certificate issuance config" }
func (ccmIssuanceProvider) CreateLayout() string { return console.CreateLayoutPage }

func (ccmIssuanceProvider) CreateForm() (string, []console.Field) {
	fields := []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: computeNamePattern},
		{Name: "description", Label: "Description", Type: "text", Help: "Optional."},
	}
	fields = append(fields, ccmLocationFields("")...)
	return "Create", append(fields,
		console.Field{Name: "caPool", Label: "CA pool", Type: "text", Required: true, Help: "A Certificate Authority Service CA pool: projects/P/locations/L/caPools/POOL."},
		console.Field{Name: "lifetime", Label: "Certificate lifetime (days)", Type: "text", Default: "30", Pattern: `^[0-9]+$`, Help: "From 21 to 30 days."},
		console.Field{Name: "rotation", Label: "Rotation window percentage", Type: "text", Default: "34", Pattern: `^[0-9]+$`,
			Help: "How far into its lifetime a certificate is renewed, as a percentage."},
		console.Field{Name: "keyAlgorithm", Label: "Key algorithm", Type: "select", Default: "RSA_2048", Options: []string{"RSA_2048", "ECDSA_P256"}},
	)
}

func (p ccmIssuanceProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	name := strings.TrimSpace(v["name"])
	days, err := strconv.Atoi(vpcOr(strings.TrimSpace(v["lifetime"]), "30"))
	if err != nil {
		return "", errors.New("Certificate lifetime: a number of days")
	}
	rot, err := strconv.Atoi(vpcOr(strings.TrimSpace(v["rotation"]), "34"))
	if err != nil {
		return "", errors.New("Rotation window percentage: a number")
	}
	body := map[string]any{"certificateAuthorityConfig": map[string]any{"certificateAuthorityServiceConfig": map[string]any{"caPool": strings.TrimSpace(v["caPool"])}},
		"lifetime": strconv.Itoa(days*86400) + "s", "rotationWindowPercentage": rot, "keyAlgorithm": vpcOr(v["keyAlgorithm"], "RSA_2048")}
	if d := strings.TrimSpace(v["description"]); d != "" {
		body["description"] = d
	}
	if err := p.ccmCreate(ctx, project, ccmLocation(v), "certificateIssuanceConfigs", "certificateIssuanceConfigId", name, body); err != nil {
		return "", err
	}
	return name, nil
}

func (p ccmIssuanceProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 2 || action != "delete" {
		return fmt.Errorf("%s is not available here", action)
	}
	_, err := p.v1(ctx, "DELETE", "/v1/"+ccmName(project, path, "certificateIssuanceConfigs"), nil)
	return err
}
