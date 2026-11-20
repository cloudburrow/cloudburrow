package main

// Cloud SQL's pages laid out as Google's console lays them out (#1860):
// an instance's page has Google's menu (Overview, System insights, Query
// insights, Connections, Users, Databases, Backups, Replicas, Operations,
// Logs, and Cloud SQL Studio on the servers `up` runs), its instance
// operations beside its title, and every other action on the page it
// belongs to; Create instance is Google's create page, the database
// engine first. Sources: https://docs.cloud.google.com/sql/docs/postgres/instance-info,
// https://docs.cloud.google.com/sql/docs/postgres/create-instance.

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	sqladminapi "google.golang.org/api/sqladmin/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// sqlVersionLabel is a databaseVersion as Google's console names it:
// POSTGRES_17 is "PostgreSQL 17", MYSQL_8_0 "MySQL 8.0".
func sqlVersionLabel(v string) string {
	engine, rest, ok := strings.Cut(v, "_")
	if !ok {
		return v
	}
	switch engine {
	case "POSTGRES":
		return "PostgreSQL " + strings.ReplaceAll(rest, "_", ".")
	case "MYSQL":
		return "MySQL " + strings.ReplaceAll(rest, "_", ".")
	case "SQLSERVER":
		return "SQL Server " + strings.ReplaceAll(strings.ToLower(rest), "_", " ")
	}
	return v
}

// sqlServerVersionLabel is a SQL Server version as Google's console names
// it, "SQL Server 2022 Standard", with the local edition that runs it.
func sqlServerVersionLabel(v string) string {
	ed := sqladmin.SQLServerEdition(v)
	label := "SQL Server " + sqladmin.SQLServerYear(v) + " " + strings.ToUpper(ed[:1]) + strings.ToLower(ed[1:])
	pid, _ := sqladmin.MSSQLPID(v)
	if pid != "Express" {
		label += " (runs " + sqlServerPIDLabel(pid) + " locally)"
	}
	return label
}

func sqlServerPIDLabel(pid string) string {
	switch pid {
	case "StandardDeveloper":
		return "Standard Developer"
	case "EnterpriseDeveloper":
		return "Enterprise Developer"
	}
	return pid
}

// sqlEditionLabel is a Cloud SQL edition as the console names it.
func sqlEditionLabel(e string) string {
	switch e {
	case "ENTERPRISE_PLUS":
		return "Enterprise Plus"
	case "ENTERPRISE":
		return "Enterprise"
	}
	return e
}

// sqlAvailabilityLabel is a zonal availability as the console names it.
func sqlAvailabilityLabel(a string) string {
	switch a {
	case "REGIONAL":
		return "Multiple zones (Highly available)"
	case "ZONAL":
		return "Single zone"
	}
	return a
}

// The engines of Google's "Choose your database engine", and the one
// CloudBurrow does not run.
const (
	sqlEngineMySQL     = "MySQL"
	sqlEnginePostgres  = "PostgreSQL"
	sqlEngineSQLServer = "SQL Server"
)

// sqlRegions are the regions the create page offers; the instance's region
// is reported and is part of its connection name, and every server runs
// in this cluster whichever is chosen.
var sqlRegions = []string{"us-central1", "us-east1", "us-east4", "us-west1", "us-west2", "northamerica-northeast1",
	"southamerica-east1", "europe-west1", "europe-west2", "europe-west3", "europe-west4", "europe-north1",
	"asia-east1", "asia-northeast1", "asia-south1", "asia-southeast1", "australia-southeast1"}

// sqlPresetTier is the machine type an edition preset gives when Machine
// type is left empty, as Google's presets fill the machine configuration.
func sqlPresetTier(edition, preset string) string {
	switch {
	case edition == "ENTERPRISE_PLUS" && preset == "Production":
		return "db-perf-optimized-N-8"
	case edition == "ENTERPRISE_PLUS":
		return "db-perf-optimized-N-2"
	case preset == "Production":
		return "db-custom-4-26624"
	case preset == "Development":
		return "db-custom-1-3840"
	}
	return "db-f1-micro"
}

// CreateTitle implements console.CreateTitler: the page is Google's
// "Create an instance", ending in Create Instance.
func (cloudSQLInstancesProvider) CreateTitle() string { return "Create an instance" }

// CreateOnPage implements console.PageCreator.
func (cloudSQLInstancesProvider) CreateOnPage() bool { return true }

// zipLabels pairs each option with its label, for Field.OptionLabels, which
// is keyed by the option's value.
func zipLabels(options, labels []string) map[string]string {
	out := make(map[string]string, len(options))
	for i, o := range options {
		if i < len(labels) {
			out[o] = labels[i]
		}
	}
	return out
}

// sqlCreateForm is Google's create page (#1860): the engine, the edition and
// its preset, the instance info, the region and zonal availability, then
// "Customize your instance".
func (p cloudSQLInstancesProvider) sqlCreateForm() []console.Field {
	var pg, my, ms, pgLabels, myLabels, msLabels []string
	pgDef, myDef, msDef := "", "", ""
	for _, v := range p.versions {
		switch {
		case strings.HasPrefix(v, "SQLSERVER_"):
			ms, msLabels = append(ms, v), append(msLabels, sqlServerVersionLabel(v))
			if v == "SQLSERVER_2022_STANDARD" || msDef == "" {
				msDef = v
			}
		case strings.HasPrefix(v, "POSTGRES_"):
			pg, pgLabels = append(pg, v), append(pgLabels, sqlVersionLabel(v))
			if v == "POSTGRES_17" || pgDef == "" {
				pgDef = v
			}
		case strings.HasPrefix(v, "MYSQL_"):
			my, myLabels = append(my, v), append(myLabels, sqlVersionLabel(v))
			if v == "MYSQL_8_0" || myDef == "" {
				myDef = v
			}
		}
	}
	const engineSec, editionSec, infoSec, regionSec = "Choose your database engine", "Choose a Cloud SQL edition",
		"Instance info", "Choose region and zonal availability"
	// Customize your instance's groups, in Google's order: Machine
	// configuration, Data cache, Storage, Encryption, Connections,
	// Security, Data protection, Maintenance, Flags, Query insights, Labels.
	const machineSec, storageSec, encryptSec, connSec, securitySec, protectSec = "Machine configuration", "Storage",
		"Encryption", "Connections", "Security", "Data protection"
	const maintSec, flagsSec, insightsSec, labelsSec, externalSec = "Maintenance", "Flags", "Query insights", "Labels",
		"Source representation (external server)"
	pgOnly := map[string]string{"engine": sqlEnginePostgres}
	msOnly := map[string]string{"engine": sqlEngineSQLServer}
	engineLabels := []string{"MySQL", "PostgreSQL", "SQL Server"}
	var disabled []string
	engineHelp := "SQL Server runs Microsoft's SQL Server image, which needs you to accept Microsoft's licence."
	if len(ms) == 0 {
		engineLabels[2], disabled = "SQL Server (not available locally)", []string{sqlEngineSQLServer}
		engineHelp = "SQL Server is not available on this instance: its Cloud SQL Admin API is not served."
	}
	myOnly := map[string]string{"engine": sqlEngineMySQL}
	days := []string{"", "1", "2", "3", "4", "5", "6", "7"}
	dayLabels := []string{"Any window", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}
	hours := []string{""}
	hourLabels := []string{"Any hour"}
	for h := 0; h < 24; h++ {
		hours, hourLabels = append(hours, strconv.Itoa(h)), append(hourLabels, fmt.Sprintf("%02d:00 UTC", h))
	}
	return withPasswordPolicy(infoSec, []console.Field{
		{Name: "engine", Label: "Database engine", Type: "radio", Section: engineSec, Default: sqlEnginePostgres,
			Options:         []string{sqlEngineMySQL, sqlEnginePostgres, sqlEngineSQLServer},
			OptionLabels:    zipLabels([]string{sqlEngineMySQL, sqlEnginePostgres, sqlEngineSQLServer}, engineLabels),
			DisabledOptions: disabled,
			Help:            engineHelp},
		{Name: "edition", Label: "Cloud SQL edition", Type: "radio", Section: editionSec, Default: "ENTERPRISE_PLUS",
			Options: []string{"ENTERPRISE_PLUS", "ENTERPRISE"}, OptionLabels: map[string]string{"ENTERPRISE_PLUS": "Enterprise Plus", "ENTERPRISE": "Enterprise"},
			Help: "Recorded and reported, and Enterprise Plus allows 35 days of transaction logs; the server is the same either way."},
		{Name: "preset", Label: "Edition preset", Type: "radio", Section: editionSec, Default: "Development",
			Options: []string{"Production", "Development", "Sandbox"},
			Help:    "Fills the machine type when Machine configuration leaves it empty."},
		{Name: "databaseVersion", Label: "Database version", Type: "select", Required: true, Section: infoSec,
			Options: pg, OptionLabels: zipLabels(pg, pgLabels), Default: pgDef, VisibleWhen: pgOnly,
			Help: "Each version is the official image of that major version, pulled when an instance of it is first created."},
		{Name: "mysqlVersion", Label: "Database version", Type: "select", Required: true, Section: infoSec,
			Options: my, OptionLabels: zipLabels(my, myLabels), Default: myDef, VisibleWhen: myOnly,
			Help: "Each version is the official image of that major version, pulled when an instance of it is first created."},
		{Name: "sqlserverVersion", Label: "Database version", Type: "select", Required: true, Section: infoSec,
			Options: ms, OptionLabels: zipLabels(ms, msLabels), Default: msDef, VisibleWhen: msOnly,
			Help: "Each release is Microsoft's image of it, linux/amd64 only (on Apple silicon, Docker Desktop's Rosetta or QEMU runs it, " +
				"which Microsoft does not support). Express runs as Express; Web, Standard and Enterprise run Microsoft's Developer edition " +
				"(Standard Developer or Enterprise Developer on 2025), licensed for development and test only, because Google holds " +
				"those editions' licences on Cloud SQL and nobody does here."},
		{Name: "mssqlEula", Label: "I accept Microsoft's licence terms for SQL Server", Type: "checkbox", Section: infoSec, VisibleWhen: msOnly,
			Help: "Microsoft's SQL Server image starts only once its End-User Licensing Agreement is accepted, and CloudBurrow never " +
				"accepts it for you. Checking this accepts it, as you, for this instance; who accepted and when is recorded on the " +
				"instance and shown by cloudburrow status. Not needed when cloudburrow up was started with --accept-mssql-eula.",
			Links: []console.Link{{Label: "Microsoft SQL Server End-User Licensing Agreement", URL: sqladmin.MSSQLEULAURL},
				{Label: "SQL Server licensing terms, by edition", URL: sqladmin.MSSQLLicensesURL}}},
		{Name: "name", Label: "Instance ID", Type: "text", Required: true, Section: infoSec, Pattern: "^[a-z][a-z0-9\\-]{0,53}[a-z0-9]$",
			Help: "Use lowercase letters, numbers, and hyphens. Start with a letter."},
		{Name: "rootPassword", Label: "Password", Type: "password", Section: infoSec,
			Help: "The password of the default user: postgres on PostgreSQL, root on MySQL, sqlserver on SQL Server (required there). Empty leaves MySQL's root without one."},
		{Name: "region", Label: "Region", Type: "select", Section: regionSec, Options: sqlRegions, Default: "us-central1",
			Help: "Reported, and part of the connection name; every server runs in this cluster."},
		{Name: "availabilityType", Label: "Zonal availability", Type: "radio", Section: regionSec, Default: "ZONAL",
			Options: []string{"ZONAL", "REGIONAL"}, OptionLabels: map[string]string{"ZONAL": "Single zone", "REGIONAL": "Multiple zones (Highly available)"},
			Help: availabilityHelp},
		{Name: "tier", Label: "Machine type", Type: "text", Section: machineSec,
			Help: "Such as db-custom-2-7680; empty takes the edition preset's. Recorded and reported; the server is not sized by it."},
		dataCacheField("Data cache"),
		{Name: "dataDiskSizeGb", Label: "Storage capacity (GB)", Type: "number", Section: storageSec,
			Help: "Empty is 10 GB. Storage shrink, on the instance's page, makes it smaller later."},
		{Name: "encryption", Label: "Encryption", Type: "radio", Section: encryptSec, Default: sqlEncryptionGoogle,
			Options: []string{sqlEncryptionGoogle, sqlEncryptionCMEK},
			OptionLabels: map[string]string{sqlEncryptionGoogle: "Google-managed encryption key",
				sqlEncryptionCMEK: "Customer-managed encryption key (CMEK)"},
			Help: "Google-managed: no key to manage. CMEK: a key of this instance's Cloud KMS, which you control."},
		{Name: "kmsKeyName", Label: "Customer-managed encryption key", Type: "text", Section: encryptSec,
			VisibleWhen: map[string]string{"encryption": sqlEncryptionCMEK}, Required: true,
			Help: "projects/P/locations/REGION/keyRings/R/cryptoKeys/K, a key of this instance's Cloud KMS in the instance's region (needs kms in --services). " +
				"The instance is SUSPENDED while the key's version is disabled or destroyed. The key cannot be changed later, and the server's data volume is not encrypted with it."},
		{Name: "publicIp", Label: "Public IP", Type: "checkbox", Section: connSec, Default: "true",
			Help: "Instance IP assignment (ipv4Enabled). Every connection reaches the server through its local tunnel or the connectors, whatever this says."},
		{Name: "authorizedNetworks", Label: "Authorized networks", Type: "text", Section: connSec,
			VisibleWhen: map[string]string{"publicIp": "true"},
			Help:        "CIDR ranges allowed to connect, separated by commas, such as 203.0.113.0/24. Recorded and shown on Connections > Networking."},
		serverCAModeSection(serverCAModeField(internalCAMode), securitySec),
		serverCAModeSection(serverCAPoolFields(nil)[0], securitySec), serverCAModeSection(serverCAPoolFields(nil)[1], securitySec),
		{Name: "backupsEnabled", Label: "Automate daily backups", Type: "checkbox", Section: protectSec, Default: "true"},
		{Name: "pitr", Label: "Enable point-in-time recovery", Type: "checkbox", Section: protectSec, Default: "true",
			Help: "Keeps the transaction log, PostgreSQL's archived WAL or MySQL's binary log, so a clone can be restored to any time since the instance started."},
		{Name: "deletionProtection", Label: "Enable deletion protection", Type: "checkbox", Section: protectSec, Default: "true",
			Help: "The instance cannot be deleted until this is turned off."},
		{Name: "maintenanceDay", Label: "Window", Type: "select", Section: maintSec, Options: days, OptionLabels: zipLabels(days, dayLabels),
			Help: "The day of the week maintenance may restart the instance."},
		{Name: "maintenanceHour", Label: "Time", Type: "select", Section: maintSec, Options: hours, OptionLabels: zipLabels(hours, hourLabels)},
		{Name: "flags", Label: "Database flags", Type: "text", Section: flagsSec,
			Help: "name=value pairs, separated by commas, passed to the server."},
		{Name: "queryInsightsEnabled", Label: "Enable Query insights", Type: "checkbox", Section: insightsSec,
			Help: "Recorded on the instance (settings.insightsConfig). The Query insights page itself is not available locally."},
		{Name: "labels", Label: "Labels", Type: "text", Section: labelsSec, Help: "key=value pairs, separated by commas."},
		{Name: "sourceHostPort", Label: "External server host:port", Type: "text", Section: externalSec,
			Help: "Set to record a MySQL or PostgreSQL server outside Cloud SQL as a source representation instance (ON_PREMISES_INSTANCE) instead: " +
				"no server runs for it; create a read replica of it, then Start external sync. Another CloudBurrow instance is sqlinst-NAME:3306 (MySQL) or sqlinst-NAME:5432 (PostgreSQL)."},
		{Name: "sourceUser", Label: "Replication user", Type: "text", Section: externalSec,
			Help: "The user the replica connects to the external server as."},
		{Name: "sourcePassword", Label: "Replication password", Type: "password", Section: externalSec},
		{Name: "sourceDatabases", Label: "Databases to migrate (PostgreSQL)", Type: "text", Section: externalSec,
			Help: "Comma-separated databases (selectedObjects); empty migrates every database but the templates."},
	})
}

// The create page's Encryption choices.
const sqlEncryptionGoogle, sqlEncryptionCMEK = "GOOGLE_MANAGED", "CMEK"

// sqlCreateConnections sets the create page's Connections and Query
// insights on st: Public IP (ipv4Enabled), the authorized networks, and
// insightsConfig. It keeps the ipConfiguration the Security group set.
func sqlCreateConnections(st *sqladminapi.Settings, v map[string]string) error {
	if pub, nets := v["publicIp"], strings.TrimSpace(v["authorizedNetworks"]); pub != "" || nets != "" {
		ic := st.IpConfiguration
		if ic == nil {
			ic = &sqladminapi.IpConfiguration{}
		}
		ic.Ipv4Enabled = pub != "false"
		ic.ForceSendFields = append(ic.ForceSendFields, "Ipv4Enabled")
		for _, n := range strings.Split(nets, ",") {
			if n = strings.TrimSpace(n); n == "" {
				continue
			}
			if _, _, err := net.ParseCIDR(n); err != nil {
				if net.ParseIP(n) == nil {
					return fmt.Errorf("authorized network %q is not a CIDR range", n)
				}
			}
			ic.AuthorizedNetworks = append(ic.AuthorizedNetworks, &sqladminapi.AclEntry{Value: n, Kind: "sql#aclEntry"})
		}
		st.IpConfiguration = ic
	}
	if v["queryInsightsEnabled"] == "true" {
		st.InsightsConfig = &sqladminapi.InsightsConfig{QueryInsightsEnabled: true}
	}
	return nil
}

func serverCAModeSection(f console.Field, section string) console.Field {
	f.Section = section
	return f
}

// sqlCreateValues turns the create page's values into what Create sends:
// the engine's version, the preset's machine type.
func sqlCreateValues(v map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(v))
	for k, val := range v {
		out[k] = val
	}
	switch v["engine"] {
	case sqlEngineSQLServer:
		out["databaseVersion"] = v["sqlserverVersion"]
		if out["databaseVersion"] == "" {
			return nil, fmt.Errorf("choose a SQL Server version")
		}
		// Not served on SQL Server here (#1916): the create page's
		// defaults are left out rather than refused.
		out["backupsEnabled"], out["pitr"] = "false", "false"
	case sqlEngineMySQL:
		out["databaseVersion"] = v["mysqlVersion"]
	}
	if strings.TrimSpace(out["tier"]) == "" {
		out["tier"] = sqlPresetTier(v["edition"], v["preset"])
	}
	return out, nil
}

// sqlCreateSettings adds the create page's Customize settings to st.
func sqlCreateSettings(st *sqladminapi.Settings, v map[string]string) error {
	if e := v["edition"]; e != "" {
		st.Edition = e
	}
	if s := strings.TrimSpace(v["dataDiskSizeGb"]); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			return fmt.Errorf("storage capacity %q is not a number of GB", s)
		}
		st.DataDiskSizeGb = n
	}
	if d, h := v["maintenanceDay"], v["maintenanceHour"]; d != "" || h != "" {
		mw := &sqladminapi.MaintenanceWindow{}
		mw.Day, _ = strconv.ParseInt(d, 10, 64)
		mw.Hour, _ = strconv.ParseInt(h, 10, 64)
		mw.ForceSendFields = []string{"Day", "Hour"}
		st.MaintenanceWindow = mw
	}
	if s := strings.TrimSpace(v["labels"]); s != "" {
		pairs, err := parsePairs(s)
		if err != nil {
			return fmt.Errorf("labels: %w", err)
		}
		st.UserLabels = map[string]string{}
		for _, l := range pairs {
			st.UserLabels[l[0]] = l[1]
		}
	}
	if s := strings.TrimSpace(v["flags"]); s != "" {
		pairs, err := parsePairs(s)
		if err != nil {
			return fmt.Errorf("database flags: %w", err)
		}
		for _, f := range pairs {
			st.DatabaseFlags = append(st.DatabaseFlags, &sqladminapi.DatabaseFlags{Name: f[0], Value: f[1]})
		}
	}
	return nil
}

// sqlPlace says where an instance page's action is drawn: "" beside the
// title, or the ID of the menu page it belongs to. Google's page-level
// buttons are the instance's operations; Create backup, Create database and
// Add user account are on their pages.
var sqlPlace = map[string]string{
	"stop": "", "start": "", "import": "", "export": "", "restart": "", "clone": "", "failover": "", "promote-replica": "", "upgrade": "",
	"delete-instance": "", "delete-final-backup": "",

	"create-client-cert": "connections", "delete-client-cert": "connections", "reset-ssl": "connections",
	"add-server-ca": "connections", "rotate-server-ca": "connections", "add-server-cert": "connections",
	"rotate-server-cert": "connections", "resolve-dns": "connections",

	"add-user": "users", "add-iam-user": "users", "change-password": "users", "remove-user": "users",
	"create-database": "databases", "drop-database": "databases",
	"create-backup": "backups", "restore-backup": "backups", "delete-backup": "backups",
	"pitr-restore": "backups", "binlog-clone": "backups",
	// The project's Backups API names the same backups (#1860): its
	// actions are on Backups too.
	"create-project-backup": "backups", "edit-project-backup": "backups",
	"restore-project-backup": "backups", "delete-project-backup": "backups",

	"create-replica": "replicas", "stop-replica": "replicas", "start-replica": "replicas",
	"designate-dr-replica": "replicas", "clear-dr-replica": "replicas", "switchover": "replicas",
	"blue-green-create": "replicas", "blue-green-switchover": "replicas", "blue-green-delete": "replicas",
	"verify-external-sync": "replicas", "start-external-sync": "replicas", "demote": "replicas",
}

// sqlRowActions are the actions drawn on each row of a menu page rather
// than over it: Google's databases have a trash can, a user Change password
// and Remove, a backup Restore and Delete. The page's own action of the
// same ID stays the one the server checks; the row's carries the row's
// name in a hidden field.
var sqlRowActions = map[string]struct{ label, field string }{
	"drop-database": {"Delete", "dbName"}, "change-password": {"Change password", "user"},
	"remove-user": {"Remove", "user"}, "restore-backup": {"Restore", "backup"}, "delete-backup": {"Delete", "backup"},
}

// sqlOverviewGroups puts each summary property on its Overview card.
var sqlOverviewGroups = map[string]string{
	"Connection name": "Connect to this instance", "Public IP address": "Connect to this instance",
	"DNS name": "Connect to this instance", "Server CAs": "Connect to this instance",
	"Server CA mode": "Connect to this instance", "Server CA pool": "Connect to this instance",
	"Automated backups": "Data protection", "Backup start time (UTC)": "Data protection",
	"Retained automated backups": "Data protection", "Point-in-time recovery": "Data protection",
	"Transaction log retention (days)": "Data protection", "Earliest recovery time": "Data protection",
	"Latest recovery time": "Data protection", "Deletion protection": "Data protection", "Backup location": "Data protection",
	"Instance type": "Replication", "Primary instance": "Replication", "Read replicas": "Replication",
	"DR replica": "Replication", "Disaster recovery role": "Replication",
	"Blue-green deployment": "Replication", "Blue-green deployment state": "Replication",
	"Maintenance window (UTC)": "Maintenance", "Scheduled maintenance": "Maintenance", "Reschedule deadline": "Maintenance",
}

var sqlOverviewOrder = []string{"Connect to this instance", "Configuration", "Data protection", "Replication", "Maintenance"}

// googleLayout lays out an instance's page (#1860) from the page every
// action of it is on: the summary becomes the Overview's cards, the tabs
// Google's menu, and each action moves to its page.
func googleLayout(ctx context.Context, svc *sqladminapi.Service, project string, in *sqladminapi.DatabaseInstance, d console.Detail) console.Detail {
	out := console.Detail{Title: d.Title, Edit: d.Edit, SideNav: true, Actions: []console.Action{}}
	// Overview: Google's cards.
	cards := map[string][]console.Property{}
	for _, pr := range d.Summary {
		g := sqlOverviewGroups[pr.Label]
		if g == "" {
			g = "Configuration"
		}
		switch pr.Label {
		case "Version":
			pr = console.Property{Label: "Database version", Value: sqlVersionLabel(pr.Value)}
		case "Tier":
			pr.Label = "Machine type"
		case "Zonal availability":
			pr = console.Property{Label: "High availability", Value: sqlAvailabilityLabel(pr.Value)}
		case "Connection name":
			pr.Copy = "Copy"
		}
		cards[g] = append(cards[g], pr)
	}
	if st := in.Settings; st != nil && st.Edition != "" {
		cards["Configuration"] = append([]console.Property{{Label: "Cloud SQL edition", Value: sqlEditionLabel(st.Edition)}}, cards["Configuration"]...)
	}
	cards["Connect to this instance"] = append(sqlAddresses(in), cards["Connect to this instance"]...)
	if in.ServiceAccountEmailAddress != "" {
		cards["Service account"] = []console.Property{{Label: "Service account", Value: in.ServiceAccountEmailAddress}}
	}
	overview := console.Section{ID: "overview", Label: "Overview", Kind: console.KindProperties}
	for _, h := range append(sqlOverviewOrder[:1:1], append([]string{"Service account"}, sqlOverviewOrder[1:]...)...) {
		if len(cards[h]) > 0 {
			overview.Groups = append(overview.Groups, console.PropertyGroup{Heading: h, Properties: cards[h]})
		}
	}
	notLocal := func(id, label, why string) console.Section {
		return console.Section{ID: id, Label: label, Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{Heading: "Not available locally", Properties: []console.Property{{Label: label, Value: why}}}}}
	}
	sections := []console.Section{overview,
		notLocal("system-insights", "System insights", "System insights charts Cloud Monitoring metrics, which CloudBurrow does not emulate. "+
			"The instance's operations are on Operations and its log lines in Logs."),
		notLocal("query-insights", "Query insights", "Query insights is not available locally: it reads Cloud SQL's query telemetry, which CloudBurrow does not collect. "+
			"pg_stat_statements or performance_schema can be queried with any client.")}
	byID := map[string]console.Section{}
	for _, s := range d.Sections {
		byID[s.ID] = s
	}
	// Connections: Google's Summary, Networking and Security tabs, the
	// client certificates on Security (#1860).
	if c, ok := byID["client-certificates"]; ok {
		byID["connections"] = sqlConnectionsSection(in, &c)
	} else {
		byID["connections"] = sqlConnectionsSection(in, nil)
	}
	// Backups: the instance's backups and the project's of it, one list,
	// as Google's Backups page has them (#1860).
	if b, ok := byID["backups"]; ok {
		byID["backups"] = sqlMergedBackups(b, byID["project-backups"])
		delete(byID, "project-backups")
	}
	byID["replicas"] = sqlReplicasSection(in)
	byID["operations"] = sqlOperationsSection(ctx, svc, project, in.Name)
	order := []string{"connections", "users", "databases", "backups", "replicas", "operations"}
	for _, id := range order {
		if s, ok := byID[id]; ok {
			sections = append(sections, s)
		}
	}
	if upSQLInstance(in.Name) {
		studio := "/cloudsql"
		if strings.HasPrefix(in.DatabaseVersion, "MYSQL_") {
			studio = "/cloudsql-mysql"
		}
		sections = append(sections, console.Section{ID: "studio", Label: "Cloud SQL Studio", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{Heading: "Cloud SQL Studio", Properties: []console.Property{
				{Label: "Explore and query the databases", Value: "Open Cloud SQL Studio", Link: studio}}}}})
	}
	index := map[string]int{}
	for i, s := range sections {
		index[s.ID] = i
	}
	for _, a := range d.Actions {
		place, known := sqlPlace[a.ID]
		if !known {
			place = "overview"
		}
		if ra, ok := sqlRowActions[a.ID]; ok {
			if i, ok := index[place]; ok {
				sqlAttachRowAction(&sections[i], in.Name, a, ra.label, ra.field)
				continue
			}
		}
		if i, ok := index[place]; ok && place != "" {
			sections[i].Actions = append(sections[i].Actions, a)
			continue
		}
		out.Actions = append(out.Actions, a)
	}
	// The certificate and server CA actions are on Connections' Security
	// tab, as Google's Manage client certificates and server CAs are.
	if i, ok := index["connections"]; ok {
		c := &sections[i]
		for j := range c.Tabs {
			if c.Tabs[j].ID == "connections-security" {
				c.Tabs[j].Actions, c.Actions = append(c.Tabs[j].Actions, c.Actions...), nil
			}
		}
	}
	out.Sections = sections
	return out
}

// sqlAttachRowAction offers a on each row of s, its field set to the row.
func sqlAttachRowAction(s *console.Section, instance string, a console.Action, label, field string) {
	for i := range s.Listing.Items {
		r := &s.Listing.Items[i]
		row := console.Action{ID: a.ID, Label: label, Destructive: a.Destructive, Confirm: a.Confirm, Submit: a.Submit,
			ConfirmTitle: a.ConfirmTitle, ConfirmStyle: a.ConfirmStyle}
		row.Fields = []console.Field{{Name: field, Type: "hidden", Default: r.Name}}
		for _, f := range a.Fields {
			if f.Name != field {
				row.Fields = append(row.Fields, f)
			}
		}
		r.Actions = append(r.Actions, row)
		r.ActsOn = []string{instance}
	}
}

// sqlAddresses are the instance's IP addresses, Public and Private as
// Google's Overview names them.
func sqlAddresses(in *sqladminapi.DatabaseInstance) []console.Property {
	var out []console.Property
	for _, ip := range in.IpAddresses {
		label := "Public IP address"
		if ip.Type == "PRIVATE" {
			label = "Private IP address"
		} else if ip.Type == "OUTGOING" {
			label = "Outgoing IP address"
		}
		out = append(out, console.Property{Label: label, Value: ip.IpAddress, Copy: "Copy"})
	}
	return out
}

// sqlReplicasSection is the Replicas page: the instance and its read
// replicas, each opening its own page.
func sqlReplicasSection(in *sqladminapi.DatabaseInstance) console.Section {
	l := console.Listing{Columns: []string{"Role"}, NameColumn: "Instance ID", Noun: "replicas"}
	if in.MasterInstanceName != "" {
		_, primary, _ := strings.Cut(in.MasterInstanceName, ":")
		if primary == "" {
			primary = in.MasterInstanceName
		}
		l.Items = append(l.Items, console.Resource{Name: primary, Link: "/cloudsql-instances/" + primary,
			Fields: map[string]string{"Role": "Primary"}})
	}
	for _, r := range in.ReplicaNames {
		role := "Read replica"
		if drReplicaOf(in) == r {
			role = "Read replica (DR replica)"
		}
		l.Items = append(l.Items, console.Resource{Name: r, Link: "/cloudsql-instances/" + r, Fields: map[string]string{"Role": role}})
	}
	l.Total = len(l.Items)
	return console.Section{ID: "replicas", Label: "Replicas", Kind: console.KindListing, Listing: l}
}

// sqlOperationsSection is the Operations page: the instance's operations,
// newest first, through operations.list.
func sqlOperationsSection(ctx context.Context, svc *sqladminapi.Service, project, name string) console.Section {
	l := console.Listing{Columns: []string{"Type", "Start time", "End time", "Error"}, NameColumn: "Operation", Noun: "operations",
		AlwaysStatus: true}
	ops, err := svc.Operations.List(project).Instance(name).MaxResults(50).Context(ctx).Do()
	if err != nil {
		l.Unavailable = sqlAPIError(err).Error()
	} else {
		for _, op := range ops.Items {
			msg := ""
			if op.Error != nil && len(op.Error.Errors) > 0 {
				msg = op.Error.Errors[0].Message
			}
			l.Items = append(l.Items, console.Resource{Name: op.Name, Status: op.Status, Fields: map[string]string{
				"Type": op.OperationType, "Start time": op.StartTime, "End time": op.EndTime, "Error": msg}})
		}
		l.Total = len(l.Items)
	}
	return console.Section{ID: "operations", Label: "Operations", Kind: console.KindListing, Listing: l}
}

// instanceOps are the page's own instance operations Google puts beside
// the title that the page did not have: Restart and Delete.
func instanceOps(in *sqladminapi.DatabaseInstance) []console.Action {
	if upSQLInstance(in.Name) || in.InstanceType == "ON_PREMISES_INSTANCE" {
		return nil
	}
	return append(stopStartActions(in),
		console.Action{ID: "restart", Label: "Restart", Confirm: "Restarting the instance closes every connection to it."},
		console.Action{ID: "delete-instance", Label: "Delete", Destructive: true, Leaves: true,
			Confirm: "The instance, its databases and its automated backups are deleted."})
}

// cloudSQLBackupsProvider is Cloud SQL's Backups page (#1860): the
// project's backups, every instance's, through the Backups API.
type cloudSQLBackupsProvider struct{ instances cloudSQLInstancesProvider }

func (cloudSQLBackupsProvider) ID() string    { return "cloudsql-backups" }
func (cloudSQLBackupsProvider) Title() string { return "Cloud SQL backups" }

func (p cloudSQLBackupsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Instance", "Type", "Location", "Description", "Created", "Expires"}, NameColumn: "Backup",
		Noun: "backups", AlwaysStatus: true,
		Note: "The project's backups (the Backups API), every instance's, a FINAL backup whose instance is gone among them. " +
			"An instance's automated and on-demand backups are on its own Backups page."}
	svc, err := p.instances.api(ctx)
	if err != nil {
		l.Unavailable = err.Error()
		return l, nil
	}
	list, err := svc.Backups.ListBackups("projects/" + project).Context(ctx).Do()
	if err != nil {
		l.Unavailable = "cannot reach the Cloud SQL Admin API: " + sqlAPIError(err).Error()
		return l, nil
	}
	for _, b := range list.Backups {
		created := ""
		if b.BackupInterval != nil {
			created = b.BackupInterval.StartTime
		}
		r := console.Resource{Name: lastSegment(b.Name), Status: b.State, Fields: map[string]string{
			"Instance": b.Instance, "Type": b.Type, "Location": b.Location, "Description": b.Description,
			"Created": created, "Expires": b.ExpiryTime}}
		if b.Instance != "" {
			r.Link = "/cloudsql-instances/" + b.Instance + "?tab=project-backups"
		}
		l.Items = append(l.Items, r)
	}
	l.Total = len(l.Items)
	return l, nil
}
