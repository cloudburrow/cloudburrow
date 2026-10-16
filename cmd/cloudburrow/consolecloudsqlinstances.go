package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	sqladminapi "google.golang.org/api/sqladmin/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// cloudSQLInstancesProvider is Cloud SQL's Instances page (#1330): every
// instance the Admin API serves, the servers `up` runs and the ones created
// through instances.insert, with Create instance and Delete; each
// instance's page has Edit instance (instances.patch) on a created one, and
// a Backups tab with Create backup, Restore and Delete. It reads and
// writes through the Admin API itself, so the page shows what the API and
// gcloud and Terraform see.
type cloudSQLInstancesProvider struct {
	// endpoint is the Admin API's host:port.
	endpoint string
	// versions are the databaseVersion values the instance's images provide.
	versions []string
}

func (cloudSQLInstancesProvider) ID() string    { return "cloudsql-instances" }
func (cloudSQLInstancesProvider) Title() string { return "Cloud SQL instances" }

const cloudSQLInstancesNote = "Each created instance is a server of its own, from the official image of its " +
	"database version (PostgreSQL 13 to 18, MySQL 8.0 or 8.4), reached through the connectors on port 3307. The instances `up` runs " +
	"(postgres, mysql) cannot be edited or deleted here. A backup is a dump of the server's databases, kept on the host; " +
	"an instance with REGIONAL availability has a synchronous standby, and Failover makes it the primary under the same connection name. " +
	"IAM database authentication does not exist here."

func (p cloudSQLInstancesProvider) api(ctx context.Context) (*sqladminapi.Service, error) {
	return sqladminapi.NewService(ctx, option.WithEndpoint("http://"+p.endpoint+"/"), option.WithoutAuthentication(),
		option.WithHTTPClient(http.DefaultClient))
}

func (p cloudSQLInstancesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{
		Columns:    []string{"Connection name", "Version", "Tier", "Address", "Availability", "Deletion protection"},
		NameColumn: "Instance ID",
		Noun:       "instances",
		Note:       cloudSQLInstancesNote,
	}
	svc, err := p.api(ctx)
	if err != nil {
		l.Unavailable = err.Error()
		return l, nil
	}
	resp, err := svc.Instances.List(project).Context(ctx).Do()
	if err != nil {
		l.Unavailable = "cannot reach the Cloud SQL Admin API: " + err.Error()
		return l, nil
	}
	for _, in := range resp.Items {
		addr := ""
		if len(in.IpAddresses) > 0 {
			addr = in.IpAddresses[0].IpAddress
		}
		tier, protect, avail := "", "Off", ""
		if in.Settings != nil {
			tier, avail = in.Settings.Tier, in.Settings.AvailabilityType
			if in.Settings.DeletionProtectionEnabled {
				protect = "On"
			}
		}
		l.Items = append(l.Items, console.Resource{Name: in.Name, Status: in.State, Fields: map[string]string{
			"Connection name": in.ConnectionName, "Version": in.DatabaseVersion, "Tier": tier,
			"Address": addr, "Deletion protection": protect, "Availability": avail,
		}})
	}
	l.Total = len(l.Items)
	return l, nil
}

// CreateForm implements console.Creator: what instances.insert takes here.
func (p cloudSQLInstancesProvider) CreateForm() (string, []console.Field) {
	def := ""
	for _, v := range p.versions {
		if def == "" || v == "POSTGRES_17" {
			def = v
		}
	}
	return "Create instance", []console.Field{
		{Name: "name", Label: "Instance ID", Type: "text", Required: true, Pattern: "^[a-z][a-z0-9-]{0,53}[a-z0-9]$",
			Help: "Lowercase letters, digits and hyphens, starting with a letter."},
		{Name: "databaseVersion", Label: "Database version", Type: "select", Required: true, Options: p.versions, Default: def,
			Help: "Each version is the official image of that major version, pulled when an instance of it is first created."},
		{Name: "region", Label: "Region", Type: "text", Default: "us-central1", Pattern: "^[a-z]+(-[a-z]+)+[0-9]+$",
			Help: "Reported, and part of the connection name; every server runs in this cluster."},
		{Name: "tier", Label: "Machine type (tier)", Type: "text", Required: true, Default: "db-custom-1-3840",
			Help: "Recorded and reported; the server is not sized by it."},
		{Name: "rootPassword", Label: "Root password (MySQL)", Type: "password",
			Help: "MySQL's root password. Empty leaves root without one, as Cloud SQL does. PostgreSQL's postgres user is trusted locally."},
		{Name: "availabilityType", Label: "Zonal availability", Type: "select", Required: true,
			Options: []string{"ZONAL", "REGIONAL"}, Default: "ZONAL", Help: availabilityHelp},
		{Name: "deletionProtection", Label: "Deletion protection", Type: "checkbox"},
		{Name: "pitr", Label: "Point-in-time recovery", Type: "checkbox",
			Help: "Keeps the transaction log, PostgreSQL's archived WAL or MySQL's binary log, so a clone can be restored to any time since the instance started."},
	}
}

// availabilityHelp explains the Zonal availability choice (#1408).
const availabilityHelp = "ZONAL is a single zone. REGIONAL (highly available) adds a standby in a second zone, " +
	"replicated synchronously, so a commit returns once the standby has it and a failover loses no committed transaction."

// pitrConfig sets bc's point-in-time recovery switch, which Cloud SQL
// names by engine: pointInTimeRecoveryEnabled for PostgreSQL,
// binaryLogEnabled for MySQL.
func pitrConfig(bc *sqladminapi.BackupConfiguration, version string, on bool) {
	if strings.HasPrefix(version, "MYSQL_") {
		bc.BinaryLogEnabled = on
		bc.ForceSendFields = append(bc.ForceSendFields, "BinaryLogEnabled")
		return
	}
	bc.PointInTimeRecoveryEnabled = on
	bc.ForceSendFields = append(bc.ForceSendFields, "PointInTimeRecoveryEnabled")
}

// retentionConfig sets bc's retained backups and transaction log days
// from the Edit form; an empty field leaves the setting out.
func retentionConfig(bc *sqladminapi.BackupConfiguration, retained, days string) error {
	if retained = strings.TrimSpace(retained); retained != "" {
		n, err := strconv.ParseInt(retained, 10, 64)
		if err != nil {
			return fmt.Errorf("retained automated backups: %q is not a number", retained)
		}
		bc.BackupRetentionSettings = &sqladminapi.BackupRetentionSettings{RetentionUnit: "COUNT", RetainedBackups: n}
	}
	if days = strings.TrimSpace(days); days != "" {
		n, err := strconv.ParseInt(days, 10, 64)
		if err != nil {
			return fmt.Errorf("transaction log retention: %q is not a number", days)
		}
		bc.TransactionLogRetentionDays = n
	}
	return nil
}

func pitrOn(st *sqladminapi.Settings) bool {
	return st != nil && st.BackupConfiguration != nil &&
		(st.BackupConfiguration.PointInTimeRecoveryEnabled || st.BackupConfiguration.BinaryLogEnabled)
}

// Create implements console.Creator. It returns once the API has accepted
// the instance; the page shows it PENDING_CREATE until its server serves.
func (p cloudSQLInstancesProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	svc, err := p.api(ctx)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(v["name"])
	st := &sqladminapi.Settings{Tier: strings.TrimSpace(v["tier"]), DeletionProtectionEnabled: v["deletionProtection"] == "true",
		AvailabilityType: v["availabilityType"]}
	if v["pitr"] == "true" {
		st.BackupConfiguration = &sqladminapi.BackupConfiguration{Enabled: true}
		pitrConfig(st.BackupConfiguration, v["databaseVersion"], true)
	}
	_, err = svc.Instances.Insert(project, &sqladminapi.DatabaseInstance{
		Name: name, DatabaseVersion: v["databaseVersion"], RootPassword: v["rootPassword"], Region: regionOr(v["region"]), Settings: st,
	}).Context(ctx).Do()
	if err != nil {
		return "", sqlAPIError(err)
	}
	return name, nil
}

// Delete implements console.Deleter: deletion protection and the instances
// `up` runs refuse it with the API's own words.
func (p cloudSQLInstancesProvider) Delete(ctx context.Context, project, name string) error {
	svc, err := p.api(ctx)
	if err != nil {
		return err
	}
	if _, err := svc.Instances.Delete(project, name).Context(ctx).Do(); err != nil {
		return sqlAPIError(err)
	}
	return nil
}

// sqlAPIError is the Admin API's own message for a failure.
func sqlAPIError(err error) error {
	var ge *googleapi.Error
	if errors.As(err, &ge) && ge.Message != "" {
		return fmt.Errorf("%s", ge.Message)
	}
	return err
}

// Actions implements console.Actor: Restart, on a created instance.
func (p cloudSQLInstancesProvider) Actions(r console.Resource) []console.Action {
	if r.Name == sqlAdminPostgresInstance || r.Name == sqlAdminMySQLInstance {
		return nil
	}
	return []console.Action{{ID: "restart", Label: "Restart", Confirm: "Restarting the instance closes every connection to it."}}
}

// Act implements console.Actor.
func (p cloudSQLInstancesProvider) Act(ctx context.Context, project, name, action string) error {
	if action != "restart" {
		return fmt.Errorf("unknown action %q", action)
	}
	svc, err := p.api(ctx)
	if err != nil {
		return err
	}
	if _, err := svc.Instances.Restart(project, name).Context(ctx).Do(); err != nil {
		return sqlAPIError(err)
	}
	return nil
}

func upSQLInstance(name string) bool {
	return name == sqlAdminPostgresInstance || name == sqlAdminMySQLInstance
}

// Detail implements console.Driller: an instance's settings, its Backups
// tab, Edit instance on a created one, and the backup actions.
func (p cloudSQLInstancesProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	name := path[0]
	svc, err := p.api(ctx)
	if err != nil {
		return console.Detail{}, err
	}
	in, err := svc.Instances.Get(project, name).Context(ctx).Do()
	if err != nil {
		return console.Detail{}, sqlAPIError(err)
	}
	st := in.Settings
	if st == nil {
		st = &sqladminapi.Settings{}
	}
	protect, backups, start := "Off", "Off", ""
	logDays, retained := "", ""
	if st.DeletionProtectionEnabled {
		protect = "On"
	}
	if bc := st.BackupConfiguration; bc != nil {
		if bc.Enabled {
			backups = "On"
		}
		start = bc.StartTime
		if bc.TransactionLogRetentionDays > 0 {
			logDays = strconv.FormatInt(bc.TransactionLogRetentionDays, 10)
		}
		if rs := bc.BackupRetentionSettings; rs != nil && rs.RetainedBackups > 0 {
			retained = strconv.FormatInt(rs.RetainedBackups, 10)
		}
	}
	pitr, earliest, latest := "Off", "", ""
	if pitrOn(st) {
		pitr = "On"
		if r, err := svc.Projects.Instances.GetLatestRecoveryTime(project, name).Context(ctx).Do(); err == nil {
			earliest, latest = r.EarliestRecoveryTime, r.LatestRecoveryTime
		}
	}
	d := console.Detail{Title: name}
	// A setting the instance does not have is left out, not shown blank.
	for _, pr := range []console.Property{
		{Label: "State", Value: in.State}, {Label: "Connection name", Value: in.ConnectionName},
		{Label: "Version", Value: in.DatabaseVersion}, {Label: "Tier", Value: st.Tier},
		{Label: "Zonal availability", Value: st.AvailabilityType}, {Label: "Zone", Value: in.GceZone},
		{Label: "Secondary zone", Value: in.SecondaryGceZone},
		{Label: "Labels", Value: formatPairs(st.UserLabels)}, {Label: "Database flags", Value: formatFlags(st.DatabaseFlags)},
		{Label: "Deletion protection", Value: protect}, {Label: "Automated backups", Value: backups},
		{Label: "Backup start time (UTC)", Value: start}, {Label: "Retained automated backups", Value: retained},
		{Label: "Point-in-time recovery", Value: pitr}, {Label: "Transaction log retention (days)", Value: logDays},
		{Label: "Earliest recovery time", Value: earliest}, {Label: "Latest recovery time", Value: latest},
		{Label: "Instance type", Value: in.InstanceType}, {Label: "Primary instance", Value: in.MasterInstanceName},
		{Label: "Read replicas", Value: strings.Join(in.ReplicaNames, ", ")},
	} {
		if pr.Value != "" {
			d.Summary = append(d.Summary, pr)
		}
	}
	runs := console.Listing{Columns: []string{"Type", "Description", "Enqueued", "Finished"}, NameColumn: "Backup ID", Noun: "backups",
		Note: "Each backup is the server's databases dumped with pg_dump or mysqldump. An automated backup is taken at its start time only while CloudBurrow runs."}
	var ids []string
	if list, err := svc.BackupRuns.List(project, name).Context(ctx).Do(); err != nil {
		runs.Unavailable = sqlAPIError(err).Error()
	} else {
		for _, r := range list.Items {
			id := strconv.FormatInt(r.Id, 10)
			runs.Items = append(runs.Items, console.Resource{Name: id, Status: r.Status, Fields: map[string]string{
				"Type": r.Type, "Description": r.Description, "Enqueued": r.EnqueuedTime, "Finished": r.EndTime}})
			if r.Status == "SUCCESSFUL" {
				ids = append(ids, id)
			}
		}
		runs.Total = len(runs.Items)
	}
	certs := console.Listing{Columns: []string{"SHA-1 fingerprint", "Expires"}, NameColumn: "Common name", Noun: "client certificates",
		Note: "Client certificates signed by the instance's server CA (sslCerts). The private key is shown once, when the certificate is created."}
	var fps []string
	if list, err := svc.SslCerts.List(project, name).Context(ctx).Do(); err != nil {
		certs.Unavailable = sqlAPIError(err).Error()
	} else {
		for _, c := range list.Items {
			certs.Items = append(certs.Items, console.Resource{Name: c.CommonName, Fields: map[string]string{
				"SHA-1 fingerprint": c.Sha1Fingerprint, "Expires": c.ExpirationTime}})
			fps = append(fps, c.Sha1Fingerprint)
		}
		certs.Total = len(certs.Items)
	}
	if cas, err := svc.Instances.ListServerCas(project, name).Context(ctx).Do(); err == nil {
		var fps []string
		for _, c := range cas.Certs {
			if c.Sha1Fingerprint == cas.ActiveVersion {
				fps = append(fps, c.Sha1Fingerprint+" (active)")
			} else {
				fps = append(fps, c.Sha1Fingerprint)
			}
		}
		d.Summary = append(d.Summary, console.Property{Label: "Server CAs", Value: strings.Join(fps, ", ")})
	}
	d.Sections = []console.Section{{ID: "backups", Label: "Backups", Kind: console.KindListing, Listing: runs},
		{ID: "client-certificates", Label: "Client certificates", Kind: console.KindListing, Listing: certs}}
	d.Actions = append(append(append(p.backupActions(ids), replicaActions(in)...), transferActions()...), certActions(fps)...)
	d.Actions = append(d.Actions, upgradeActions(in)...)
	d.Actions = append(d.Actions, failoverActions(in)...)
	d.Actions = append(d.Actions, dataAPIActions(ctx, svc, project, in)...)
	if pitr == "On" && strings.HasPrefix(in.DatabaseVersion, "MYSQL_") {
		d.Actions = append(d.Actions, console.Action{ID: "binlog-clone", Label: "Clone to binary log position", Fields: []console.Field{
			{Name: "destination", Label: "New instance ID", Type: "text", Required: true, Pattern: "^[a-z][a-z0-9-]{0,53}[a-z0-9]$",
				Help: "A new instance with this instance's databases as they were at the binary log position below."},
			{Name: "binLogFileName", Label: "Binary log file", Type: "text", Required: true, Help: "As SHOW BINARY LOG STATUS names it, such as binlog.000002."},
			{Name: "binLogPosition", Label: "Position", Type: "number", Required: true, Help: "Every transaction that starts before it is cloned."}}})
	}
	if pitr == "On" {
		d.Actions = append(d.Actions, console.Action{ID: "pitr-restore", Label: "Point-in-time restore", Fields: []console.Field{
			{Name: "destination", Label: "New instance ID", Type: "text", Required: true, Pattern: "^[a-z][a-z0-9-]{0,53}[a-z0-9]$",
				Help: "A new instance with this instance's settings and its databases as they were at the time below."},
			{Name: "pointInTime", Label: "Point in time (UTC, RFC 3339)", Type: "text", Required: true, Default: latest,
				Help: "Between the earliest recovery time, " + earliest + ", and now, to the microsecond."}}})
	}
	if !upSQLInstance(name) {
		d.Edit = &console.EditForm{Label: "Edit instance", Fields: []console.Field{
			{Name: "tier", Label: "Machine type (tier)", Type: "text", Required: true, Default: st.Tier,
				Help: "Recorded and reported; the server is not sized by it."},
			{Name: "labels", Label: "Labels", Type: "text", Default: formatPairs(st.UserLabels),
				Help: "key=value pairs, separated by commas. Empty removes every label."},
			{Name: "flags", Label: "Database flags", Type: "text", Default: formatFlags(st.DatabaseFlags),
				Help: "name=value pairs, separated by commas, passed to the server, which restarts with them."},
			{Name: "deletionProtection", Label: "Deletion protection", Type: "checkbox", Default: strconv.FormatBool(st.DeletionProtectionEnabled)},
			{Name: "availabilityType", Label: "Zonal availability", Type: "select", Required: true,
				Options: []string{"ZONAL", "REGIONAL"}, Default: availabilityOr(st.AvailabilityType),
				Help: availabilityHelp + " Changing it adds or removes the standby."},
			{Name: "backupsEnabled", Label: "Automated backups", Type: "checkbox", Default: strconv.FormatBool(backups == "On")},
			{Name: "backupStartTime", Label: "Backup start time (UTC, HH:MM)", Type: "text", Default: start, Pattern: "^([01][0-9]|2[0-3]):[0-5][0-9]$"},
			{Name: "pitr", Label: "Point-in-time recovery", Type: "checkbox", Default: strconv.FormatBool(pitr == "On"),
				Help: "PostgreSQL's server restarts to archive its WAL; recovery reaches back to the first base taken after it is on."},
			{Name: "retainedBackups", Label: "Retained automated backups", Type: "number", Default: retained,
				Help: "1 to 365; older automated backups are deleted."},
			{Name: "logRetentionDays", Label: "Transaction log retention (days)", Type: "number", Default: logDays,
				Help: "1 to 7 (35 on Enterprise Plus); older log and bases are dropped, and the earliest recovery time moves with them."},
		}, Note: "The database version and the instance ID cannot be changed."}
	}
	return d, nil
}

func formatPairs(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return strings.Join(out, ",")
}

func formatFlags(fs []*sqladminapi.DatabaseFlags) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Name+"="+f.Value)
	}
	return strings.Join(out, ",")
}

func parsePairs(s string) ([][2]string, error) {
	var out [][2]string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("%q is not name=value", part)
		}
		out = append(out, [2]string{strings.TrimSpace(k), strings.TrimSpace(v)})
	}
	return out, nil
}

// Edit implements console.Editor: one instances.patch with every setting
// the form holds.
func (p cloudSQLInstancesProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("%s", console.DeeperThan(1, path).Unavailable)
	}
	labels, err := parsePairs(v["labels"])
	if err != nil {
		return fmt.Errorf("labels: %w", err)
	}
	flags, err := parsePairs(v["flags"])
	if err != nil {
		return fmt.Errorf("database flags: %w", err)
	}
	st := &sqladminapi.Settings{Tier: strings.TrimSpace(v["tier"]), UserLabels: map[string]string{},
		DatabaseFlags:             []*sqladminapi.DatabaseFlags{},
		DeletionProtectionEnabled: v["deletionProtection"] == "true",
		AvailabilityType:          v["availabilityType"],
		BackupConfiguration: &sqladminapi.BackupConfiguration{Enabled: v["backupsEnabled"] == "true",
			StartTime: strings.TrimSpace(v["backupStartTime"]), ForceSendFields: []string{"Enabled"}},
		ForceSendFields: []string{"DeletionProtectionEnabled", "UserLabels", "DatabaseFlags"}}
	for _, l := range labels {
		st.UserLabels[l[0]] = l[1]
	}
	for _, f := range flags {
		st.DatabaseFlags = append(st.DatabaseFlags, &sqladminapi.DatabaseFlags{Name: f[0], Value: f[1]})
	}
	svc, err := p.api(ctx)
	if err != nil {
		return err
	}
	in, err := svc.Instances.Get(project, path[0]).Context(ctx).Do()
	if err != nil {
		return sqlAPIError(err)
	}
	pitrConfig(st.BackupConfiguration, in.DatabaseVersion, v["pitr"] == "true")
	if err := retentionConfig(st.BackupConfiguration, v["retainedBackups"], v["logRetentionDays"]); err != nil {
		return err
	}
	if _, err := svc.Instances.Patch(project, path[0], &sqladminapi.DatabaseInstance{Settings: st}).Context(ctx).Do(); err != nil {
		return sqlAPIError(err)
	}
	return nil
}

// replicaActions are Create read replica on a primary, and Promote, Stop
// replication and Start replication on a read replica (#1330).
func replicaActions(in *sqladminapi.DatabaseInstance) []console.Action {
	create := console.Action{ID: "create-replica", Label: "Create read replica", Fields: []console.Field{{Name: "replica",
		Label: "Replica instance ID", Type: "text", Required: true, Pattern: "^[a-z][a-z0-9-]{0,53}[a-z0-9]$",
		Help: "A new instance replicating from this one by the engine's own replication; it takes no writes. " +
			"A replica of a replica is a cascading replica (#1381)."},
		{Name: "region", Label: "Region", Type: "text", Default: in.Region, Pattern: "^[a-z]+(-[a-z]+)+[0-9]+$",
			Help: "Another region makes a cross-region replica; the server runs in this cluster either way."}}}
	if in.MasterInstanceName == "" {
		return []console.Action{create}
	}
	acts := []console.Action{create, {ID: "promote-replica", Label: "Promote", Destructive: true,
		Confirm: "Promoting stops replication from the primary for good: the replica becomes a standalone primary that takes writes."}}
	if st := in.Settings; st != nil && !st.DatabaseReplicationEnabled {
		return append(acts, console.Action{ID: "start-replica", Label: "Start replication"})
	}
	return append(acts, console.Action{ID: "stop-replica", Label: "Stop replication",
		Confirm: "The replica stops applying the primary's changes until replication is started again."})
}

// upgradeActions is Upgrade, a major version upgrade through
// instances.patch with a newer databaseVersion (#1381), offered with the
// instance's upgradableDatabaseVersions.
func upgradeActions(in *sqladminapi.DatabaseInstance) []console.Action {
	var opts []string
	for _, v := range in.UpgradableDatabaseVersions {
		opts = append(opts, v.MajorVersion)
	}
	if len(opts) == 0 {
		return nil
	}
	return []console.Action{{ID: "upgrade", Label: "Upgrade", Destructive: true,
		Confirm: "The server is replaced by one of the new major version and its data loaded into it; connections are closed meanwhile.",
		Fields: []console.Field{{Name: "databaseVersion", Label: "Database version", Type: "select", Required: true,
			Options: opts, Default: opts[len(opts)-1]},
			{Name: "includeReplicas", Label: "Include replicas", Type: "checkbox", Default: "false",
				Help: "Upgrade the read replicas too, after the primary (includeReplicasForMajorVersionUpgrade); an instance with replicas needs it."}}}}
}

// failoverActions is Failover on a REGIONAL instance (#1408): its standby
// becomes the primary behind the same connection name.
func failoverActions(in *sqladminapi.DatabaseInstance) []console.Action {
	if in.Settings == nil || in.Settings.AvailabilityType != "REGIONAL" {
		return nil
	}
	return []console.Action{{ID: "failover", Label: "Failover", Destructive: true,
		Confirm: "The standby becomes the primary, under the same connection name; connections are closed, " +
			"and the old primary is rebuilt as the new standby."}}
}

func availabilityOr(a string) string {
	if a == "" {
		return "ZONAL"
	}
	return a
}

// transferActions are Export and Import (#1380), to and from the
// instance's Cloud Storage, through instances.export and instances.import.
func transferActions() []console.Action {
	uri := console.Field{Name: "uri", Label: "Cloud Storage file", Type: "text", Required: true, Pattern: "^gs://[^/]+/.+$",
		Help: "gs://bucket/file in this instance's Cloud Storage; a name ending in .gz is gzip (SQL only)."}
	fileType := console.Field{Name: "fileType", Label: "Format", Type: "select", Required: true, Options: []string{"SQL", "CSV"}, Default: "SQL"}
	db := console.Field{Name: "database", Label: "Database", Type: "text",
		Help: "Required on PostgreSQL, and for CSV; a MySQL SQL export without one exports every database."}
	parallel := console.Field{Name: "parallel", Label: "Parallel (SQL)", Type: "checkbox", Default: "false",
		Help: "A folder of files, pg_dump's directory format or MySQL Shell's dump, written or read with several threads; the file above is the folder."}
	threads := console.Field{Name: "threads", Label: "Threads (parallel)", Type: "number", Help: "Empty is 4."}
	return []console.Action{
		{ID: "export", Label: "Export", Fields: []console.Field{uri, fileType, db,
			{Name: "selectQuery", Label: "SQL query (CSV)", Type: "text", Help: "The SELECT whose rows a CSV export writes."},
			parallel, threads,
			{Name: "masterData", Label: "Binary log coordinates (MySQL SQL)", Type: "select", Options: []string{"0", "1", "2"}, Default: "0",
				Help: "masterData: 1 writes a CHANGE REPLICATION SOURCE TO statement, 2 the same as a comment."}}},
		{ID: "import", Label: "Import", Confirm: "The file's statements or rows are run in, or loaded into, the database.",
			Fields: []console.Field{uri, fileType, db,
				{Name: "table", Label: "Table (CSV)", Type: "text", Help: "The table a CSV import loads its rows into."},
				parallel, threads,
				{Name: "clean", Label: "Clean (PostgreSQL parallel)", Type: "checkbox", Default: "false", Help: "pg_restore --clean: drop each object before it is restored."},
				{Name: "ifExists", Label: "If exists (PostgreSQL parallel)", Type: "checkbox", Default: "false", Help: "pg_restore --if-exists, with Clean."}}},
	}
}

// consoleThreads is a parallel transfer's Threads field: empty is the
// server's default.
func consoleThreads(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("threads %q is not a positive number", v)
	}
	return n, nil
}

// certActions are Create client certificate, Delete client certificate and
// Reset SSL configuration (#1380), through sslCerts.insert, sslCerts.delete
// and instances.resetSslConfig.
func certActions(fps []string) []console.Action {
	acts := []console.Action{{ID: "create-client-cert", Label: "Create client certificate", Fields: []console.Field{{Name: "commonName",
		Label: "Common name", Type: "text", Required: true, Help: "A name no other client certificate of the instance has."}}}}
	if len(fps) > 0 {
		acts = append(acts, console.Action{ID: "delete-client-cert", Label: "Delete client certificate", Destructive: true,
			Confirm: "The certificate is deleted.", Fields: []console.Field{{Name: "fingerprint", Label: "SHA-1 fingerprint",
				Type: "select", Required: true, Options: fps, Default: fps[0]}}})
	}
	return append(acts, console.Action{ID: "reset-ssl", Label: "Reset SSL configuration", Destructive: true,
		Confirm: "Every client certificate of the instance is deleted, and the server's certificate is issued anew."},
		console.Action{ID: "add-server-ca", Label: "Add server CA",
			Confirm: "A new upcoming server CA is made; one added before and never rotated in is replaced."},
		console.Action{ID: "rotate-server-ca", Label: "Rotate server CA",
			Confirm: "The upcoming server CA becomes the one the server's certificate is signed by; connectors fetch it on their next dial."})
}

func (p cloudSQLInstancesProvider) backupActions(ids []string) []console.Action {
	acts := []console.Action{{ID: "create-backup", Label: "Create backup",
		Fields: []console.Field{{Name: "description", Label: "Description", Type: "text"}}},
		{ID: "clone", Label: "Clone", Fields: []console.Field{{Name: "destination", Label: "New instance ID", Type: "text", Required: true,
			Pattern: "^[a-z][a-z0-9-]{0,53}[a-z0-9]$", Help: "A new instance with this instance's settings and a copy of its databases as they are now."}}}}
	if len(ids) > 0 {
		acts = append(acts,
			console.Action{ID: "restore-backup", Label: "Restore", Destructive: true,
				Confirm: "Restoring replaces every database of the instance with the backup's; what was written since is lost.",
				Fields:  []console.Field{{Name: "backup", Label: "Backup ID", Type: "select", Required: true, Options: ids, Default: ids[0]}}},
			console.Action{ID: "delete-backup", Label: "Delete backup", Destructive: true, Confirm: "The backup's dump is deleted.",
				Fields: []console.Field{{Name: "backup", Label: "Backup ID", Type: "select", Required: true, Options: ids, Default: ids[0]}}})
	}
	return acts
}

// DetailActions implements console.PathActor.
func (p cloudSQLInstancesProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	d, err := p.Detail(ctx, project, path)
	if err != nil {
		return nil
	}
	return d.Actions
}

// ActAt implements console.PathActor: the backup actions, through
// backupRuns.insert, instances.restoreBackup and backupRuns.delete, Clone,
// through instances.clone, Point-in-time restore, through instances.clone
// with cloneContext.pointInTime, the replica actions, through
// instances.insert, promoteReplica, stopReplica and startReplica, Failover,
// through instances.failover with the instance's settingsVersion, and
// Export and Import, through instances.export and instances.import.
func (p cloudSQLInstancesProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	_, err := p.ActAtResult(ctx, project, path, action, v)
	return err
}

// ActAtResult implements console.ResultActor: Create client certificate
// answers with the certificate, its private key and the server CA, which
// sslCerts.insert returns once; every other action with nothing.
func (p cloudSQLInstancesProvider) ActAtResult(ctx context.Context, project string, path []string, action string, v map[string]string) (*console.Listing, error) {
	if (action == "run-sql" || action == "precheck-upgrade") && len(path) == 1 {
		svc, err := p.api(ctx)
		if err != nil {
			return nil, err
		}
		out, _, err := dataAPIResult(ctx, svc, project, path[0], action, v)
		return out, err
	}
	if action != "create-client-cert" || len(path) != 1 {
		return nil, p.actAt(ctx, project, path, action, v)
	}
	svc, err := p.api(ctx)
	if err != nil {
		return nil, err
	}
	res, err := svc.SslCerts.Insert(project, path[0], &sqladminapi.SslCertsInsertRequest{CommonName: strings.TrimSpace(v["commonName"])}).Context(ctx).Do()
	if err != nil {
		return nil, sqlAPIError(err)
	}
	out := &console.Listing{Columns: []string{"PEM"}, NameColumn: "File", Noun: "files"}
	add := func(name, pem string) {
		out.Items = append(out.Items, console.Resource{Name: name, Fields: map[string]string{"PEM": pem}})
	}
	if res.ClientCert != nil && res.ClientCert.CertInfo != nil {
		add("client-cert.pem", res.ClientCert.CertInfo.Cert)
		add("client-key.pem", res.ClientCert.CertPrivateKey)
	}
	if res.ServerCaCert != nil {
		add("server-ca.pem", res.ServerCaCert.Cert)
	}
	out.Total = len(out.Items)
	return out, nil
}

func (p cloudSQLInstancesProvider) actAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("%s", console.DeeperThan(1, path).Unavailable)
	}
	name := path[0]
	svc, err := p.api(ctx)
	if err != nil {
		return err
	}
	var id int64
	if action == "restore-backup" || action == "delete-backup" {
		if id, err = strconv.ParseInt(strings.TrimSpace(v["backup"]), 10, 64); err != nil {
			return fmt.Errorf("backup ID %q is not a number", v["backup"])
		}
	}
	switch action {
	case "create-backup":
		_, err = svc.BackupRuns.Insert(project, name, &sqladminapi.BackupRun{Description: strings.TrimSpace(v["description"])}).Context(ctx).Do()
	case "restore-backup":
		_, err = svc.Instances.RestoreBackup(project, name, &sqladminapi.InstancesRestoreBackupRequest{
			RestoreBackupContext: &sqladminapi.RestoreBackupContext{BackupRunId: id, InstanceId: name, Project: project}}).Context(ctx).Do()
	case "clone":
		_, err = svc.Instances.Clone(project, name, &sqladminapi.InstancesCloneRequest{
			CloneContext: &sqladminapi.CloneContext{DestinationInstanceName: strings.TrimSpace(v["destination"])}}).Context(ctx).Do()
	case "pitr-restore":
		_, err = svc.Instances.Clone(project, name, &sqladminapi.InstancesCloneRequest{
			CloneContext: &sqladminapi.CloneContext{DestinationInstanceName: strings.TrimSpace(v["destination"]),
				PointInTime: strings.TrimSpace(v["pointInTime"])}}).Context(ctx).Do()
	case "binlog-clone":
		var pos int64
		if pos, err = strconv.ParseInt(strings.TrimSpace(v["binLogPosition"]), 10, 64); err != nil {
			return fmt.Errorf("position %q is not a number", v["binLogPosition"])
		}
		_, err = svc.Instances.Clone(project, name, &sqladminapi.InstancesCloneRequest{
			CloneContext: &sqladminapi.CloneContext{DestinationInstanceName: strings.TrimSpace(v["destination"]),
				BinLogCoordinates: &sqladminapi.BinLogCoordinates{BinLogFileName: strings.TrimSpace(v["binLogFileName"]), BinLogPosition: pos}}}).Context(ctx).Do()
	case "delete-backup":
		_, err = svc.BackupRuns.Delete(project, name, id).Context(ctx).Do()
	case "create-replica":
		_, err = svc.Instances.Insert(project, &sqladminapi.DatabaseInstance{Name: strings.TrimSpace(v["replica"]),
			MasterInstanceName: name, InstanceType: "READ_REPLICA_INSTANCE", Region: strings.TrimSpace(v["region"])}).Context(ctx).Do()
	case "upgrade":
		_, err = svc.Instances.Patch(project, name, &sqladminapi.DatabaseInstance{DatabaseVersion: v["databaseVersion"],
			IncludeReplicasForMajorVersionUpgrade: v["includeReplicas"] == "true"}).Context(ctx).Do()
	case "failover":
		var in *sqladminapi.DatabaseInstance
		if in, err = svc.Instances.Get(project, name).Context(ctx).Do(); err == nil {
			var sv int64
			if in.Settings != nil {
				sv = in.Settings.SettingsVersion
			}
			_, err = svc.Instances.Failover(project, name, &sqladminapi.InstancesFailoverRequest{
				FailoverContext: &sqladminapi.FailoverContext{SettingsVersion: sv}}).Context(ctx).Do()
		}
	case "promote-replica":
		_, err = svc.Instances.PromoteReplica(project, name).Context(ctx).Do()
	case "stop-replica":
		_, err = svc.Instances.StopReplica(project, name).Context(ctx).Do()
	case "start-replica":
		_, err = svc.Instances.StartReplica(project, name).Context(ctx).Do()
	case "export":
		x := &sqladminapi.ExportContext{Uri: strings.TrimSpace(v["uri"]), FileType: v["fileType"]}
		if d := strings.TrimSpace(v["database"]); d != "" {
			x.Databases = []string{d}
		}
		if x.FileType == "CSV" {
			x.CsvExportOptions = &sqladminapi.ExportContextCsvExportOptions{SelectQuery: strings.TrimSpace(v["selectQuery"])}
		} else {
			o := &sqladminapi.ExportContextSqlExportOptions{Parallel: v["parallel"] == "true"}
			if o.Parallel {
				if o.Threads, err = consoleThreads(v["threads"]); err != nil {
					return err
				}
			}
			if md := strings.TrimSpace(v["masterData"]); md == "1" || md == "2" {
				o.MysqlExportOptions = &sqladminapi.ExportContextSqlExportOptionsMysqlExportOptions{MasterData: int64(md[0] - '0')}
			}
			if o.Parallel || o.MysqlExportOptions != nil {
				x.SqlExportOptions = o
			}
		}
		_, err = svc.Instances.Export(project, name, &sqladminapi.InstancesExportRequest{ExportContext: x}).Context(ctx).Do()
	case "import":
		x := &sqladminapi.ImportContext{Uri: strings.TrimSpace(v["uri"]), FileType: v["fileType"], Database: strings.TrimSpace(v["database"])}
		if x.FileType == "CSV" {
			x.CsvImportOptions = &sqladminapi.ImportContextCsvImportOptions{Table: strings.TrimSpace(v["table"])}
		} else if v["parallel"] == "true" {
			o := &sqladminapi.ImportContextSqlImportOptions{Parallel: true}
			if o.Threads, err = consoleThreads(v["threads"]); err != nil {
				return err
			}
			if v["clean"] == "true" || v["ifExists"] == "true" {
				o.PostgresImportOptions = &sqladminapi.ImportContextSqlImportOptionsPostgresImportOptions{
					Clean: v["clean"] == "true", IfExists: v["ifExists"] == "true"}
			}
			x.SqlImportOptions = o
		}
		_, err = svc.Instances.Import(project, name, &sqladminapi.InstancesImportRequest{ImportContext: x}).Context(ctx).Do()
	case "delete-client-cert":
		_, err = svc.SslCerts.Delete(project, name, strings.TrimSpace(v["fingerprint"])).Context(ctx).Do()
	case "reset-ssl":
		_, err = svc.Instances.ResetSslConfig(project, name).Context(ctx).Do()
	case "add-server-ca":
		_, err = svc.Instances.AddServerCa(project, name).Context(ctx).Do()
	case "rotate-server-ca":
		_, err = svc.Instances.RotateServerCa(project, name, &sqladminapi.InstancesRotateServerCaRequest{}).Context(ctx).Do()
	default:
		if ok, err := dataAPIAct(ctx, svc, project, name, action, v); ok {
			return err
		}
		return fmt.Errorf("unknown action %q", action)
	}
	if err != nil {
		return sqlAPIError(err)
	}
	return nil
}

var (
	_ console.Driller     = cloudSQLInstancesProvider{}
	_ console.Editor      = cloudSQLInstancesProvider{}
	_ console.PathActor   = cloudSQLInstancesProvider{}
	_ console.ResultActor = cloudSQLInstancesProvider{}
	_ console.Actor       = cloudSQLInstancesProvider{}
	_ console.Creator     = cloudSQLInstancesProvider{}
	_ console.Deleter     = cloudSQLInstancesProvider{}
)

func regionOr(r string) string {
	if r = strings.TrimSpace(r); r != "" {
		return r
	}
	return "us-central1"
}
