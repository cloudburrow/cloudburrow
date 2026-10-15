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

const cloudSQLInstancesNote = "Each created instance is a server of its own, from the image `up` runs " +
	"(PostgreSQL 17 or MySQL 8.4), reached through the connectors on port 3307. The instances `up` runs " +
	"(postgres, mysql) cannot be edited or deleted here. A backup is a dump of the server's databases, kept on the host; " +
	"point-in-time recovery, replicas, IAM authentication and failover do not exist here."

func (p cloudSQLInstancesProvider) api(ctx context.Context) (*sqladminapi.Service, error) {
	return sqladminapi.NewService(ctx, option.WithEndpoint("http://"+p.endpoint+"/"), option.WithoutAuthentication(),
		option.WithHTTPClient(http.DefaultClient))
}

func (p cloudSQLInstancesProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{
		Columns:    []string{"Connection name", "Version", "Tier", "Address", "Deletion protection"},
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
		tier, protect := "", "Off"
		if in.Settings != nil {
			tier = in.Settings.Tier
			if in.Settings.DeletionProtectionEnabled {
				protect = "On"
			}
		}
		l.Items = append(l.Items, console.Resource{Name: in.Name, Status: in.State, Fields: map[string]string{
			"Connection name": in.ConnectionName, "Version": in.DatabaseVersion, "Tier": tier,
			"Address": addr, "Deletion protection": protect,
		}})
	}
	l.Total = len(l.Items)
	return l, nil
}

// CreateForm implements console.Creator: what instances.insert takes here.
func (p cloudSQLInstancesProvider) CreateForm() (string, []console.Field) {
	def := ""
	if len(p.versions) > 0 {
		def = p.versions[0]
	}
	return "Create instance", []console.Field{
		{Name: "name", Label: "Instance ID", Type: "text", Required: true, Pattern: "^[a-z][a-z0-9-]{0,53}[a-z0-9]$",
			Help: "Lowercase letters, digits and hyphens, starting with a letter."},
		{Name: "databaseVersion", Label: "Database version", Type: "select", Required: true, Options: p.versions, Default: def,
			Help: "The versions of the images this instance runs."},
		{Name: "tier", Label: "Machine type (tier)", Type: "text", Required: true, Default: "db-custom-1-3840",
			Help: "Recorded and reported; the server is not sized by it."},
		{Name: "rootPassword", Label: "Root password (MySQL)", Type: "password",
			Help: "MySQL's root password. Empty leaves root without one, as Cloud SQL does. PostgreSQL's postgres user is trusted locally."},
		{Name: "deletionProtection", Label: "Deletion protection", Type: "checkbox"},
	}
}

// Create implements console.Creator. It returns once the API has accepted
// the instance; the page shows it PENDING_CREATE until its server serves.
func (p cloudSQLInstancesProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	svc, err := p.api(ctx)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(v["name"])
	_, err = svc.Instances.Insert(project, &sqladminapi.DatabaseInstance{
		Name: name, DatabaseVersion: v["databaseVersion"], RootPassword: v["rootPassword"], Region: "us-central1",
		Settings: &sqladminapi.Settings{Tier: strings.TrimSpace(v["tier"]), DeletionProtectionEnabled: v["deletionProtection"] == "true"},
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
	if st.DeletionProtectionEnabled {
		protect = "On"
	}
	if bc := st.BackupConfiguration; bc != nil {
		if bc.Enabled {
			backups = "On"
		}
		start = bc.StartTime
	}
	d := console.Detail{Title: name}
	// A setting the instance does not have is left out, not shown blank.
	for _, pr := range []console.Property{
		{Label: "State", Value: in.State}, {Label: "Connection name", Value: in.ConnectionName},
		{Label: "Version", Value: in.DatabaseVersion}, {Label: "Tier", Value: st.Tier},
		{Label: "Labels", Value: formatPairs(st.UserLabels)}, {Label: "Database flags", Value: formatFlags(st.DatabaseFlags)},
		{Label: "Deletion protection", Value: protect}, {Label: "Automated backups", Value: backups},
		{Label: "Backup start time (UTC)", Value: start},
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
	d.Sections = []console.Section{{ID: "backups", Label: "Backups", Kind: console.KindListing, Listing: runs}}
	d.Actions = append(p.backupActions(ids), replicaActions(in)...)
	if !upSQLInstance(name) {
		d.Edit = &console.EditForm{Label: "Edit instance", Fields: []console.Field{
			{Name: "tier", Label: "Machine type (tier)", Type: "text", Required: true, Default: st.Tier,
				Help: "Recorded and reported; the server is not sized by it."},
			{Name: "labels", Label: "Labels", Type: "text", Default: formatPairs(st.UserLabels),
				Help: "key=value pairs, separated by commas. Empty removes every label."},
			{Name: "flags", Label: "Database flags", Type: "text", Default: formatFlags(st.DatabaseFlags),
				Help: "name=value pairs, separated by commas, passed to the server, which restarts with them."},
			{Name: "deletionProtection", Label: "Deletion protection", Type: "checkbox", Default: strconv.FormatBool(st.DeletionProtectionEnabled)},
			{Name: "backupsEnabled", Label: "Automated backups", Type: "checkbox", Default: strconv.FormatBool(backups == "On")},
			{Name: "backupStartTime", Label: "Backup start time (UTC, HH:MM)", Type: "text", Default: start, Pattern: "^([01][0-9]|2[0-3]):[0-5][0-9]$"},
		}, Note: "The database version and the instance ID cannot be changed. Point-in-time recovery is not offered."}
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
	if _, err := svc.Instances.Patch(project, path[0], &sqladminapi.DatabaseInstance{Settings: st}).Context(ctx).Do(); err != nil {
		return sqlAPIError(err)
	}
	return nil
}

// replicaActions are Create read replica on a primary, and Promote, Stop
// replication and Start replication on a read replica (#1330).
func replicaActions(in *sqladminapi.DatabaseInstance) []console.Action {
	if in.MasterInstanceName == "" {
		return []console.Action{{ID: "create-replica", Label: "Create read replica", Fields: []console.Field{{Name: "replica",
			Label: "Replica instance ID", Type: "text", Required: true, Pattern: "^[a-z][a-z0-9-]{0,53}[a-z0-9]$",
			Help: "A new instance replicating from this one by the engine's own replication; it takes no writes."}}}}
	}
	acts := []console.Action{{ID: "promote-replica", Label: "Promote", Destructive: true,
		Confirm: "Promoting stops replication from the primary for good: the replica becomes a standalone primary that takes writes."}}
	if st := in.Settings; st != nil && !st.DatabaseReplicationEnabled {
		return append(acts, console.Action{ID: "start-replica", Label: "Start replication"})
	}
	return append(acts, console.Action{ID: "stop-replica", Label: "Stop replication",
		Confirm: "The replica stops applying the primary's changes until replication is started again."})
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
// through instances.clone, and the replica actions, through
// instances.insert, promoteReplica, stopReplica and startReplica.
func (p cloudSQLInstancesProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
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
	case "delete-backup":
		_, err = svc.BackupRuns.Delete(project, name, id).Context(ctx).Do()
	case "create-replica":
		_, err = svc.Instances.Insert(project, &sqladminapi.DatabaseInstance{Name: strings.TrimSpace(v["replica"]),
			MasterInstanceName: name, InstanceType: "READ_REPLICA_INSTANCE"}).Context(ctx).Do()
	case "promote-replica":
		_, err = svc.Instances.PromoteReplica(project, name).Context(ctx).Do()
	case "stop-replica":
		_, err = svc.Instances.StopReplica(project, name).Context(ctx).Do()
	case "start-replica":
		_, err = svc.Instances.StartReplica(project, name).Context(ctx).Do()
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	if err != nil {
		return sqlAPIError(err)
	}
	return nil
}

var (
	_ console.Driller   = cloudSQLInstancesProvider{}
	_ console.Editor    = cloudSQLInstancesProvider{}
	_ console.PathActor = cloudSQLInstancesProvider{}
	_ console.Actor     = cloudSQLInstancesProvider{}
	_ console.Creator   = cloudSQLInstancesProvider{}
	_ console.Deleter   = cloudSQLInstancesProvider{}
)
