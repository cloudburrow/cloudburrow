package main

// Cloud SQL console parity, round 2 (#1860): Stop and Start beside an
// instance's title (#1875), Enable password policies on the create page and
// Edit (#1876), Connections with Google's Summary, Networking and Security
// tabs, the instance's backups in one Backups list as Google shows them,
// and Data cache, which is not served, said so. Sources:
// https://docs.cloud.google.com/sql/docs/mysql/start-stop-restart-instance,
// https://docs.cloud.google.com/sql/docs/postgres/create-instance,
// https://docs.cloud.google.com/sql/docs/postgres/built-in-authentication.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	sqladminapi "google.golang.org/api/sqladmin/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// sqlStopped is whether the instance's owner stopped it: the API answers
// RUNNABLE with activationPolicy NEVER, which Google's console and gcloud
// show as Stopped.
func sqlStopped(in *sqladminapi.DatabaseInstance) bool {
	return in.State == "RUNNABLE" && in.Settings != nil && in.Settings.ActivationPolicy == "NEVER"
}

// sqlStateLabel is the instance's state as the console shows it.
func sqlStateLabel(in *sqladminapi.DatabaseInstance) string {
	if sqlStopped(in) {
		return "STOPPED"
	}
	return in.State
}

// stopStartActions are Stop on a running created primary and Start on a
// stopped one; a read replica cannot be stopped. Each is confirmed as
// Google's is, by its own button in "Stop database instance?" or "Start
// database instance?", nothing typed.
func stopStartActions(in *sqladminapi.DatabaseInstance) []console.Action {
	if upSQLInstance(in.Name) || in.InstanceType == "ON_PREMISES_INSTANCE" || in.MasterInstanceName != "" {
		return nil
	}
	if sqlStopped(in) {
		return []console.Action{{ID: "start", Label: "Start", ConfirmTitle: "Start database instance?", ConfirmStyle: console.DeleteConfirmClick,
			Confirm: "The instance starts with its data, and accepts connections again."}}
	}
	if in.State != "RUNNABLE" {
		return nil
	}
	return []console.Action{{ID: "stop", Label: "Stop", ConfirmTitle: "Stop database instance?", ConfirmStyle: console.DeleteConfirmClick,
		Confirm: "The instance is stopped: it does not respond to connections until you start it again. Its data is kept."}}
}

// stopStart performs Stop and Start: instances.patch with
// settings.activationPolicy NEVER or ALWAYS.
func stopStart(svc *sqladminapi.Service, project, name, action string) *sqladminapi.InstancesPatchCall {
	policy := "ALWAYS"
	if action == "stop" {
		policy = "NEVER"
	}
	return svc.Instances.Patch(project, name, &sqladminapi.DatabaseInstance{Settings: &sqladminapi.Settings{ActivationPolicy: policy}})
}

// passwordPolicyFields are Google's "Enable password policies" and the
// options Set password policy offers, for the create page (section) or
// Edit (cur, the instance's policy). Restrict password reuse is MySQL 8.0
// and later's and PostgreSQL's, the change interval PostgreSQL's.
func passwordPolicyFields(section string, cur *sqladminapi.PasswordValidationPolicy, version string) []console.Field {
	if cur == nil {
		cur = &sqladminapi.PasswordValidationPolicy{}
	}
	on := map[string]string{"enablePasswordPolicy": "true"}
	num := func(n int64) string {
		if n == 0 {
			return ""
		}
		return strconv.FormatInt(n, 10)
	}
	fields := []console.Field{
		{Name: "enablePasswordPolicy", Label: "Enable password policies", Type: "checkbox", Section: section,
			Default: strconv.FormatBool(cur.EnablePasswordPolicy),
			Help:    "Users' passwords set through Add user account and Change password are checked against the policy below."},
		{Name: "passwordMinLength", Label: "Minimum length", Type: "number", Section: section, Default: num(cur.MinLength), VisibleWhen: on,
			Help: "The minimum number of characters the password must have."},
		{Name: "passwordComplexity", Label: "Password complexity", Type: "checkbox", Section: section,
			Default: strconv.FormatBool(cur.Complexity == "COMPLEXITY_DEFAULT"), VisibleWhen: on,
			Help: "A combination of lowercase, uppercase, numeric, and non-alphanumeric characters."},
		{Name: "passwordReuseInterval", Label: "Restrict password reuse", Type: "number", Section: section, Default: num(cur.ReuseInterval),
			VisibleWhen: on, Help: "The number of previous passwords that can't be reused, 0 to 100 (MySQL 8.0 and later, and PostgreSQL)."},
		{Name: "passwordDisallowUsername", Label: "Disallow username", Type: "checkbox", Section: section,
			Default: strconv.FormatBool(cur.DisallowUsernameSubstring), VisibleWhen: on,
			Help: "The password can't contain the username."},
	}
	if !strings.HasPrefix(version, "MYSQL_") {
		f := console.Field{Name: "passwordChangeInterval", Label: "Set password change interval", Type: "number", Section: section,
			VisibleWhen: on, Help: "The minimum number of hours after which the password can be changed again (PostgreSQL)."}
		if d := strings.TrimSuffix(cur.PasswordChangeInterval, "s"); d != "" && d != "0" {
			if secs, err := strconv.ParseInt(d, 10, 64); err == nil {
				f.Default = strconv.FormatInt(secs/3600, 10)
			}
		}
		if section != "" {
			// The create page's engine choice decides it.
			f.VisibleWhen = map[string]string{"enablePasswordPolicy": "true", "engine": sqlEnginePostgres}
		}
		fields = append(fields, f)
	}
	// Google's options are in the Set password policy dialog, its button
	// under Enable password policies, ending in Save.
	for i := range fields[1:] {
		fields[i+1].Panel = sqlPasswordPolicyPanel
	}
	return fields
}

// sqlPasswordPolicyPanel is Google's button, and dialog, under Enable
// password policies.
const sqlPasswordPolicyPanel = "Set password policy"

// passwordPolicyOf is the policy the form's values set, nil when the form
// enables none and the instance had none.
func passwordPolicyOf(v map[string]string, had bool) (*sqladminapi.PasswordValidationPolicy, error) {
	if v["enablePasswordPolicy"] != "true" {
		if !had {
			return nil, nil
		}
		return &sqladminapi.PasswordValidationPolicy{EnablePasswordPolicy: false, ForceSendFields: []string{"EnablePasswordPolicy"}}, nil
	}
	p := &sqladminapi.PasswordValidationPolicy{EnablePasswordPolicy: true, Complexity: "COMPLEXITY_UNSPECIFIED",
		DisallowUsernameSubstring: v["passwordDisallowUsername"] == "true",
		ForceSendFields:           []string{"EnablePasswordPolicy", "MinLength", "ReuseInterval", "DisallowUsernameSubstring"}}
	if v["passwordComplexity"] == "true" {
		p.Complexity = "COMPLEXITY_DEFAULT"
	}
	for _, f := range []struct {
		key string
		to  *int64
	}{{"passwordMinLength", &p.MinLength}, {"passwordReuseInterval", &p.ReuseInterval}} {
		if s := strings.TrimSpace(v[f.key]); s != "" {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%q is not a number", s)
			}
			*f.to = n
		}
	}
	if s := strings.TrimSpace(v["passwordChangeInterval"]); s != "" {
		h, err := strconv.ParseInt(s, 10, 64)
		if err != nil || h < 0 {
			return nil, fmt.Errorf("password change interval %q is not a number of hours", s)
		}
		p.PasswordChangeInterval = strconv.FormatInt(h*3600, 10) + "s"
	} else if v["engine"] != sqlEngineMySQL {
		p.PasswordChangeInterval = "0s"
	}
	return p, nil
}

// dataCacheField is Google's Data cache, after Machine type, on Enterprise
// Plus: data cache is not served here (no local SSD cache sits in front of
// the server), so it is offered off, its on choice not available.
func dataCacheField(section string) console.Field {
	return console.Field{Name: "dataCache", Label: "Data cache", Type: "radio", Section: section, Default: "off",
		Options: []string{"off", "on"}, OptionLabels: map[string]string{"off": "Data cache off", "on": "Enable data cache (not available locally)"},
		DisabledOptions: []string{"on"}, VisibleWhen: map[string]string{"edition": "ENTERPRISE_PLUS"},
		Help: "Data cache is not available locally: CloudBurrow runs no cache in front of the server, so reads are not accelerated."}
}

// sqlConnectionsSection is Google's Connections page: Summary, Networking
// and Security tabs, and Connectivity tests, which is not available locally.
// certs is the client certificates listing, with its actions.
func sqlConnectionsSection(in *sqladminapi.DatabaseInstance, certs *console.Section) console.Section {
	ip := in.Settings
	if ip == nil {
		ip = &sqladminapi.Settings{}
	}
	cfg := ip.IpConfiguration
	if cfg == nil {
		cfg = &sqladminapi.IpConfiguration{}
	}
	public, private := "Disabled", "Disabled"
	for _, a := range in.IpAddresses {
		switch a.Type {
		case "PRIMARY":
			public = "Enabled"
		case "PRIVATE":
			private = "Enabled"
		}
	}
	summary := []console.Property{{Label: "Connection name", Value: in.ConnectionName, Copy: "Copy"}}
	summary = append(summary, sqlAddresses(in)...)
	summary = append(summary, console.Property{Label: "DNS name", Value: in.DnsName},
		console.Property{Label: "Public IP connectivity", Value: public},
		console.Property{Label: "Private IP connectivity", Value: private},
		console.Property{Label: "SSL mode", Value: sqlSSLModeLabel(cfg.SslMode)})
	if cfg.PrivateNetwork != "" {
		n := cfg.PrivateNetwork
		summary = append(summary, console.Property{Label: "Network", Value: n[strings.LastIndex(n, "/")+1:]})
		if cfg.AllocatedIpRange != "" {
			summary = append(summary, console.Property{Label: "Allocated IP range", Value: cfg.AllocatedIpRange})
		}
	}
	if in.PscServiceAttachmentLink != "" {
		summary = append(summary, console.Property{Label: "Service attachment", Value: in.PscServiceAttachmentLink, Copy: "Copy"})
	}
	nets := console.Listing{Columns: []string{"Network", "Expires"}, NameColumn: "Name", Noun: "authorized networks",
		Note: "Authorized networks are recorded and shown: every connection reaches the server through its local tunnel or the connectors, whatever its address."}
	for i, n := range cfg.AuthorizedNetworks {
		label := n.Name
		if label == "" {
			label = "Network " + strconv.Itoa(i+1)
		}
		nets.Items = append(nets.Items, console.Resource{Name: label, Fields: map[string]string{"Network": n.Value, "Expires": orDash(n.ExpirationTime)}})
	}
	nets.Total = len(nets.Items)
	secProps := []console.Property{{Label: "SSL mode", Value: sqlSSLModeLabel(cfg.SslMode)},
		{Label: "Server CA mode", Value: cfg.ServerCaMode}, {Label: "Server CA pool", Value: cfg.ServerCaPool}}
	security := console.Section{ID: "connections-security", Label: "Security", Kind: console.KindListing}
	if certs != nil {
		security = *certs
		security.ID, security.Label = "connections-security", "Security"
		security.Note = "SSL mode " + sqlSSLModeLabel(cfg.SslMode) + ". Manage client certificates: the certificates signed by the instance's server CA (sslCerts); " +
			"the private key is shown once, when the certificate is created. The server CAs are on Overview."
	} else {
		security = console.Section{ID: "connections-security", Label: "Security", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{Heading: "Security", Properties: secProps}}}
	}
	return console.Section{ID: "connections", Label: "Connections", Kind: console.KindProperties, Tabs: []console.Section{
		{ID: "connections-summary", Label: "Summary", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{Heading: "Connect to this instance", Properties: summary}}},
		{ID: "connections-networking", Label: "Networking", Kind: console.KindListing, Listing: nets,
			Note: "Instance IP assignment: Public IP " + strings.ToLower(public) + ", private IP " + strings.ToLower(private) + ". " + nets.Note},
		security,
		{ID: "connections-tests", Label: "Connectivity tests", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{Heading: "Not available locally", Properties: []console.Property{{Label: "Connectivity tests",
				Value: "Connectivity tests are Network Intelligence Center's, which CloudBurrow does not run. Connect with the connection name through a Cloud SQL connector or the instance's local address."}}}}},
	}}
}

// sqlSSLModeLabel is an ipConfiguration.sslMode as Google's Security tab
// names it.
func sqlSSLModeLabel(m string) string {
	switch m {
	case "ENCRYPTED_ONLY":
		return "Allow only SSL connections"
	case "TRUSTED_CLIENT_CERTIFICATE_REQUIRED":
		return "Require trusted client certificates"
	case "", "ALLOW_UNENCRYPTED_AND_ENCRYPTED", "SSL_MODE_UNSPECIFIED":
		return "Allow unencrypted network traffic"
	}
	return m
}

// sqlMergedBackups is Google's Backups page of an instance: one list of
// its backups, automated, on-demand and final, each with where it is kept
// and when it expires from the project's Backups API, which names the same
// backups; a project backup of the instance that its backupRuns do not
// list (a FINAL one) is in it too.
func sqlMergedBackups(runs, proj console.Section) console.Section {
	out := runs
	l := console.Listing{Columns: []string{"Type", "Location", "Description", "Encryption key version", "Created", "Expires"},
		NameColumn: "Backup ID", Noun: "backups", AlwaysStatus: true, Unavailable: runs.Listing.Unavailable,
		Note: "The instance's backups, automated, on-demand and final; the project's Backups API names the same ones, with when each expires."}
	byID := map[string]console.Resource{}
	for _, r := range proj.Listing.Items {
		byID[r.Name] = r
	}
	seen := map[string]bool{}
	for _, r := range runs.Listing.Items {
		pr := byID[r.Name]
		created := r.Fields["Enqueued"]
		if c := pr.Fields["Created"]; c != "" {
			created = c
		}
		loc := r.Fields["Location"]
		if loc == "" {
			loc = sqlBackupLocationLabel(pr.Fields["Location"])
		}
		l.Items = append(l.Items, console.Resource{Name: r.Name, Status: r.Status, Actions: r.Actions, ActsOn: r.ActsOn,
			Fields: map[string]string{"Type": r.Fields["Type"], "Location": orDash(loc),
				"Description": r.Fields["Description"], "Encryption key version": orDash(r.Fields["Encryption key version"]),
				"Created": created, "Expires": orDash(pr.Fields["Expires"])}})
		seen[r.Name] = true
	}
	var extra []string
	for id := range byID {
		if !seen[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	for _, id := range extra {
		pr := byID[id]
		l.Items = append(l.Items, console.Resource{Name: id, Status: pr.Status, Fields: map[string]string{"Type": pr.Fields["Type"],
			"Location": orDash(sqlBackupLocationLabel(pr.Fields["Location"])), "Description": pr.Fields["Description"],
			"Encryption key version": "—", "Created": pr.Fields["Created"],
			"Expires": orDash(pr.Fields["Expires"])}})
	}
	l.Total = len(l.Items)
	out.Listing = l
	out.Kind = console.KindListing
	return out
}

// withPasswordPolicy puts Enable password policies and its options after
// the default user's password, where Google's create page has them.
func withPasswordPolicy(section string, fields []console.Field) []console.Field {
	out := make([]console.Field, 0, len(fields)+6)
	for _, f := range fields {
		out = append(out, f)
		if f.Name == "rootPassword" {
			out = append(out, passwordPolicyFields(section, nil, "")...)
		}
	}
	return out
}
