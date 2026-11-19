package main

// Cloud SQL server CA modes, server certificates and DNS names on the
// console (#1614): Server CA mode on Create instance and Edit instance; on
// a GOOGLE_MANAGED_CAS_CA or CUSTOMER_MANAGED_CAS_CA (#1834, with its CA
// pool of Certificate Authority Service) instance's page its server
// certificates, Add
// server certificate and Rotate server certificate (to the upcoming
// version, or back to the previous one); and Resolve DNS name, which
// answers connect.resolve's connect settings.

import (
	"context"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	sqladminapi "google.golang.org/api/sqladmin/v1"
)

const (
	internalCAMode = "GOOGLE_MANAGED_INTERNAL_CA"
	casCAMode      = "GOOGLE_MANAGED_CAS_CA"
	customerCAMode = "CUSTOMER_MANAGED_CAS_CA"
)

// casSigned is whether the instance's server certificate is rotated, not
// its CA: a shared or customer-managed CAS CA signs it.
func casSigned(in *sqladminapi.DatabaseInstance) bool {
	m := serverCAMode(in)
	return m == casCAMode || m == customerCAMode
}

// serverCAModeField is the Server CA mode choice of Create instance and
// Edit instance.
func serverCAModeField(def string) console.Field {
	return console.Field{Name: "serverCaMode", Label: "Server CA mode", Type: "select", Required: true,
		Options: []string{internalCAMode, casCAMode, customerCAMode}, Default: def,
		Help: "GOOGLE_MANAGED_INTERNAL_CA signs the server's certificate with a CA of the instance's own, rotated with Add server CA. " +
			"GOOGLE_MANAGED_CAS_CA signs it with the region's shared CA hierarchy (here CloudBurrow's own, local CAs), and gives the instance a DNS name; " +
			"its server certificate is rotated with Add server certificate. CUSTOMER_MANAGED_CAS_CA has a CA of your CA pool (Server CA pool) issue it. " +
			"An instance on CUSTOMER_MANAGED_CAS_CA cannot go back to GOOGLE_MANAGED_INTERNAL_CA."}
}

// serverCAPoolFields are Server CA pool and Rotate server certificates
// automatically, of Create instance and Edit instance.
func serverCAPoolFields(in *sqladminapi.DatabaseInstance) []console.Field {
	pool, rot := "", "NO_AUTOMATIC_ROTATION"
	if in != nil && in.Settings != nil && in.Settings.IpConfiguration != nil {
		pool = in.Settings.IpConfiguration.ServerCaPool
		if m := in.Settings.IpConfiguration.ServerCertificateRotationMode; m != "" {
			rot = m
		}
	}
	return []console.Field{
		{Name: "serverCaPool", Label: "Server CA pool (CUSTOMER_MANAGED_CAS_CA)", Type: "text", Default: pool,
			Help: "projects/P/locations/REGION/caPools/POOL, a CA pool of this instance's Certificate Authority Service (needs privateca in --services) " +
				"in the instance's region, with at least one enabled CA; it must allow configuration-based requests and DNS names in SANs. " +
				"Leave it empty for the other modes."},
		{Name: "serverCertificateRotationMode", Label: "Rotate server certificates automatically", Type: "select",
			Options: []string{"NO_AUTOMATIC_ROTATION", "AUTOMATIC_ROTATION_DURING_MAINTENANCE"}, Default: rot,
			Help: "With AUTOMATIC_ROTATION_DURING_MAINTENANCE a maintenance run (Schedule maintenance, then Reschedule maintenance) rotates the server certificate when it expires within 180 days; otherwise rotate with Add server certificate."},
	}
}

// serverCAMode is the instance's server CA mode.
func serverCAMode(in *sqladminapi.DatabaseInstance) string {
	if in.Settings != nil && in.Settings.IpConfiguration != nil {
		if m := in.Settings.IpConfiguration.ServerCaMode; m == casCAMode || m == customerCAMode {
			return m
		}
	}
	return internalCAMode
}

// serverCAPool is a customer-managed instance's CA pool.
func serverCAPool(in *sqladminapi.DatabaseInstance) string {
	if in.Settings != nil && in.Settings.IpConfiguration != nil {
		return in.Settings.IpConfiguration.ServerCaPool
	}
	return ""
}

// withServerCAMode sets st's server CA mode to mode, keeping the rest of
// cur, the instance's ipConfiguration; it sets nothing when mode is
// already the instance's.
func withServerCAMode(st *sqladminapi.Settings, cur *sqladminapi.DatabaseInstance, v map[string]string) {
	mode, pool, rot := v["serverCaMode"], strings.TrimSpace(v["serverCaPool"]), v["serverCertificateRotationMode"]
	if mode != customerCAMode {
		pool = ""
	}
	curRot := ""
	if cur != nil && cur.Settings != nil && cur.Settings.IpConfiguration != nil {
		curRot = cur.Settings.IpConfiguration.ServerCertificateRotationMode
	}
	if curRot == "" {
		curRot = "NO_AUTOMATIC_ROTATION"
	}
	same := cur != nil && (mode == "" || serverCAMode(cur) == mode) && serverCAPool(cur) == pool && (rot == "" || rot == curRot)
	if same || (cur == nil && (mode == "" || mode == internalCAMode) && (rot == "" || rot == "NO_AUTOMATIC_ROTATION")) {
		return
	}
	ic := &sqladminapi.IpConfiguration{}
	if cur != nil && cur.Settings != nil && cur.Settings.IpConfiguration != nil {
		c := *cur.Settings.IpConfiguration
		ic = &c
	}
	if mode != "" {
		ic.ServerCaMode = mode
	}
	ic.ServerCaPool = pool
	if pool == "" && cur != nil && serverCAPool(cur) != "" {
		// Leaving a customer's pool takes serverCaPool "", as Google
		// documents.
		ic.ForceSendFields = append(ic.ForceSendFields, "ServerCaPool")
	}
	if rot != "" {
		ic.ServerCertificateRotationMode = rot
	}
	st.IpConfiguration = ic
}

// serverCertSummary lists a CAS instance's server certificate versions,
// the active one, the upcoming one and the previous one marked, and
// returns the fingerprints of those that are not active, newest first.
func serverCertSummary(ctx context.Context, svc *sqladminapi.Service, project string, in *sqladminapi.DatabaseInstance, d *console.Detail) []string {
	if !casSigned(in) {
		return nil
	}
	list, err := svc.Instances.ListServerCertificates(project, in.Name).Context(ctx).Do()
	if err != nil {
		d.Summary = append(d.Summary, console.Property{Label: "Server certificates", Value: sqlAPIError(err).Error()})
		return nil
	}
	var active *sqladminapi.SslCert
	for _, c := range list.ServerCerts {
		if c.Sha1Fingerprint == list.ActiveVersion {
			active = c
		}
	}
	var shown, others []string
	for i := len(list.ServerCerts) - 1; i >= 0; i-- {
		c := list.ServerCerts[i]
		switch {
		case c == active:
			shown = append(shown, c.Sha1Fingerprint+" (active)")
		case active != nil && c.CreateTime > active.CreateTime:
			shown = append(shown, c.Sha1Fingerprint+" (upcoming)")
			others = append(others, c.Sha1Fingerprint)
		default:
			shown = append(shown, c.Sha1Fingerprint+" (previous)")
			others = append(others, c.Sha1Fingerprint)
		}
	}
	d.Summary = append(d.Summary, console.Property{Label: "Server certificates", Value: strings.Join(shown, ", ")})
	return others
}

// serverCertActions are a CAS instance's Add server certificate and Rotate
// server certificate, and Resolve DNS name on an instance with a DNS name.
func serverCertActions(in *sqladminapi.DatabaseInstance, others []string) []console.Action {
	var acts []console.Action
	if casSigned(in) {
		issuer := "the region's CA"
		if serverCAMode(in) == customerCAMode {
			issuer = "a CA of the instance's CA pool, " + serverCAPool(in)
		}
		acts = append(acts, console.Action{ID: "add-server-cert", Label: "Add server certificate",
			Confirm: "A new upcoming server certificate is issued by " + issuer + "; one added before and never rotated in is replaced."})
		if len(others) > 0 {
			acts = append(acts, console.Action{ID: "rotate-server-cert", Label: "Rotate server certificate",
				Confirm: "The server presents the chosen certificate from the next connection on; clients that trust its CA chain (listServerCertificates' caCerts) reach it.",
				Fields: []console.Field{{Name: "nextVersion", Label: "Certificate (SHA-1 fingerprint)", Type: "select", Required: true,
					Options: others, Default: others[0], Help: "The upcoming certificate, or the previous one to roll back to."}}})
		}
	}
	if in.DnsName != "" {
		acts = append(acts, console.Action{ID: "resolve-dns", Label: "Resolve DNS name",
			Fields: []console.Field{{Name: "dnsName", Label: "DNS name", Type: "text", Required: true, Default: in.DnsName},
				{Name: "location", Label: "Region", Type: "text", Required: true, Default: regionOr(in.Region)}}})
	}
	return acts
}

// serverCertAct runs a server certificate action; ok is false for any
// other action.
func serverCertAct(ctx context.Context, svc *sqladminapi.Service, project, name, action string, v map[string]string) (bool, error) {
	var err error
	switch action {
	case "add-server-cert":
		_, err = svc.Instances.AddServerCertificate(project, name).Context(ctx).Do()
	case "rotate-server-cert":
		_, err = svc.Instances.RotateServerCertificate(project, name, &sqladminapi.InstancesRotateServerCertificateRequest{
			RotateServerCertificateContext: &sqladminapi.RotateServerCertificateContext{NextVersion: strings.TrimSpace(v["nextVersion"])}}).Context(ctx).Do()
	default:
		return false, nil
	}
	if err != nil {
		return true, sqlAPIError(err)
	}
	return true, nil
}

// resolveDNSResult is Resolve DNS name's answer: the connect settings of
// the instance the name resolves to.
func resolveDNSResult(ctx context.Context, svc *sqladminapi.Service, v map[string]string) (*console.Listing, error) {
	cs, err := svc.Connect.Resolve(strings.TrimSpace(v["location"]), strings.TrimSpace(v["dnsName"])).Context(ctx).Do()
	if err != nil {
		return nil, sqlAPIError(err)
	}
	out := &console.Listing{Columns: []string{"Value"}, NameColumn: "Setting", Noun: "settings",
		Note: "The connect settings of the instance the DNS name resolves to (connect.resolve)."}
	for _, kv := range [][2]string{{"Connection name", cs.ConnectionName}, {"Database version", cs.DatabaseVersion},
		{"Region", cs.Region}, {"Server CA mode", cs.ServerCaMode}, {"DNS name", cs.DnsName}} {
		if kv[1] != "" {
			out.Items = append(out.Items, console.Resource{Name: kv[0], Fields: map[string]string{"Value": kv[1]}})
		}
	}
	out.Total = len(out.Items)
	return out, nil
}
