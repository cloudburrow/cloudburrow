package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Spanner's backups, backup schedules, database roles, operations, custom
// instance configurations, instance moves and partition edits in the console
// (#1498), each through the official admin clients, which the call front in
// the Spanner pod answers:
//
//   - an instance's page has a Backups tab, whose rows open a backup's page
//     ([instance, "Backups", backup]) with Update expiry, Copy backup,
//     Restore to a new database and Delete backup; Create backup; a Backup
//     operations tab and a Database operations tab;
//   - a database's page has a Backup schedules tab (Create backup schedule,
//     and each row's Edit and Delete) and a Database roles tab;
//   - the Instance configurations tab's rows open a configuration's page
//     ([instance, "instanceConfigs", config]); a Google-managed one has
//     Create custom configuration, based on it, and a custom one is renamed
//     and deleted there; Move instance is on the instance's page, beside a
//     Configuration operations tab;
//   - a partition's page has Edit instance partition.

// spannerBackups and spannerConfigs put a backup and a configuration under
// the instance whose page lists them. Neither can be a database ID, which is
// lowercase.
const (
	spannerBackups = "Backups"
	spannerConfigs = "instanceConfigs"
)

// spannerIDPattern is a backup's or restored database's ID, as Spanner takes
// it: a lowercase letter first, then lowercase letters, digits, underscores
// and hyphens, ending in a letter or digit, 2 to 30 characters in all.
const spannerIDPattern = `^[a-z][a-z0-9_\-]{0,28}[a-z0-9]$`

// spannerCustomConfigPattern is a user-managed configuration's ID, which
// Spanner requires to start with "custom-".
const spannerCustomConfigPattern = `^custom-[a-z0-9\-]{1,57}[a-z0-9]$`

var spannerCustomConfigRe = regexp.MustCompile(spannerCustomConfigPattern)

// isSpannerChildPath reports whether path is under an instance's partitions,
// backups or configurations rather than one of its databases.
func isSpannerChildPath(path []string) bool {
	return len(path) >= 2 && (path[1] == spannerPartitions || path[1] == spannerBackups || path[1] == spannerConfigs)
}

func spannerBackupName(project, instanceID, backupID string) string {
	return fmt.Sprintf("projects/%s/instances/%s/backups/%s", project, instanceID, backupID)
}

func spannerConfigName(project, configID string) string {
	if strings.Contains(configID, "/") {
		return configID
	}
	return fmt.Sprintf("projects/%s/instanceConfigs/%s", project, configID)
}

func spannerSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func spannerTS(ts *timestamppb.Timestamp) string {
	return spannerTime(ts.AsTime(), ts != nil)
}

// spannerExpiry reads a number of days from now as an expire time: Spanner
// takes 6 hours to a year (366 days).
func spannerExpiry(v string, now time.Time) (*timestamppb.Timestamp, error) {
	days, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || days <= 0 || days > 366 {
		return nil, fmt.Errorf("the expiry must be a number of days from now, more than 0 and at most 366, not %q", v)
	}
	return timestamppb.New(now.Add(time.Duration(days * 24 * float64(time.Hour)))), nil
}

func spannerExpiryField(def string) console.Field {
	return console.Field{Name: "expireDays", Label: "Expires in (days)", Type: "number", Required: true, Default: def,
		Help: "Days from now until Spanner deletes the backup: at least 6 hours (0.25), at most 366."}
}

// spannerRetention reads a backup schedule's retention, in days.
func spannerRetention(v string) (*durationpb.Duration, error) {
	days, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || days <= 0 || days > 366 {
		return nil, fmt.Errorf("the retention must be a number of days, more than 0 and at most 366, not %q", v)
	}
	return durationpb.New(time.Duration(days * 24 * float64(time.Hour))), nil
}

// spannerOperationListing lists long-running operations, each row named by
// the resource it is on and the metadata type that says what it does.
func spannerOperationListing(noun, column string, next func() (*longrunningpb.Operation, error)) console.Listing {
	ops := console.Listing{Columns: []string{column, "Type", "Done", "Error"}, NameColumn: "Operation", Noun: noun}
	for {
		o, err := next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			ops.Unavailable = "listing " + noun + ": " + err.Error()
			break
		}
		on := "—"
		if before, _, ok := strings.Cut(o.GetName(), "/operations/"); ok {
			on = lastSegment(before)
		}
		kind := "—"
		if m := o.GetMetadata(); m != nil {
			kind = lastSegment(strings.ReplaceAll(m.GetTypeUrl(), ".", "/"))
		}
		ops.Items = append(ops.Items, console.Resource{
			Name: lastSegment(o.GetName()),
			Fields: map[string]string{column: on, "Type": kind, "Done": strconv.FormatBool(o.GetDone()),
				"Error": orDash(o.GetError().GetMessage())},
		})
	}
	ops.Total = len(ops.Items)
	return ops
}

// spannerBackupListing lists an instance's backups, each opening its page.
func spannerBackupListing(ctx context.Context, dbAdmin *database.DatabaseAdminClient, instanceID, instName string) console.Listing {
	backups := console.Listing{
		Columns: []string{"Database", "Version time", "Expire time", "Size"}, NameColumn: "Backup",
		Noun: "backups", AlwaysStatus: true,
	}
	it := dbAdmin.ListBackups(ctx, &databasepb.ListBackupsRequest{Parent: instName})
	for {
		b, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			backups.Unavailable = "listing backups: " + err.Error()
			break
		}
		id := lastSegment(b.GetName())
		at := []string{instanceID, spannerBackups, id}
		backups.Items = append(backups.Items, console.Resource{
			Name: id, Status: b.GetState().String(),
			Fields: map[string]string{
				"Database":     orDash(lastSegment(b.GetDatabase())),
				"Version time": spannerTS(b.GetVersionTime()),
				"Expire time":  spannerTS(b.GetExpireTime()),
				"Size":         spannerSize(b.GetSizeBytes()),
			},
			Opens: at, ActsOn: at,
			Actions: []console.Action{spannerDeleteBackupAction(false)},
		})
	}
	sort.SliceStable(backups.Items, func(i, j int) bool { return backups.Items[i].Name < backups.Items[j].Name })
	backups.Total = len(backups.Items)
	return backups
}

// spannerBackupSections are an instance page's Backups, Backup operations
// and Database operations tabs.
func spannerBackupSections(ctx context.Context, dbAdmin *database.DatabaseAdminClient, instanceID, instName string) []console.Section {
	bops := dbAdmin.ListBackupOperations(ctx, &databasepb.ListBackupOperationsRequest{Parent: instName})
	dops := dbAdmin.ListDatabaseOperations(ctx, &databasepb.ListDatabaseOperationsRequest{Parent: instName})
	return []console.Section{
		{ID: "backups", Label: "Backups", Listing: spannerBackupListing(ctx, dbAdmin, instanceID, instName),
			Note: "Each backup holds its database's schema and rows as of its version time; open one to restore, copy or delete it."},
		{ID: "backup-operations", Label: "Backup operations", Listing: spannerOperationListing("backup operations", "Backup", bops.Next),
			Note: "The long-running operations that created and copied this instance's backups."},
		{ID: "database-operations", Label: "Database operations", Listing: spannerOperationListing("database operations", "Database", dops.Next),
			Note: "The long-running operations on this instance's databases: creates, schema changes, updates and restores."},
	}
}

// spannerConfigOperationsSection is the instance page's Instance
// configuration operations tab.
func spannerConfigOperationsSection(ctx context.Context, instAdmin *instance.InstanceAdminClient, project string) console.Section {
	it := instAdmin.ListInstanceConfigOperations(ctx, &instancepb.ListInstanceConfigOperationsRequest{Parent: "projects/" + project})
	return console.Section{ID: "config-operations", Label: "Configuration operations",
		Listing: spannerOperationListing("configuration operations", "Configuration", it.Next),
		Note:    "The long-running operations that created and changed this project's custom instance configurations."}
}

// spannerDeleteBackupAction deletes a backup, from its row or its page.
func spannerDeleteBackupAction(onPage bool) console.Action {
	return console.Action{ID: "deletebackup", Label: "Delete backup", Destructive: true, Leaves: onPage}
}

// spannerCreateBackupAction is an instance page's Create backup; databases
// are the instance's, offered as a choice when they could be listed.
func spannerCreateBackupAction(databases []string) console.Action {
	db := console.Field{Name: "database", Label: "Database", Type: "text", Required: true, Pattern: spannerIDPattern,
		Help: "The database of this instance to back up."}
	if len(databases) > 0 {
		db.Type, db.Options, db.Default, db.Pattern = "select", databases, databases[0], ""
	}
	return console.Action{ID: "createbackup", Label: "Create backup", Fields: []console.Field{
		db,
		{Name: "backup", Label: "Backup ID", Type: "text", Required: true, Pattern: spannerIDPattern,
			Help: "Lowercase letters, digits, underscores and hyphens, starting with a letter; 2 to 30 characters."},
		spannerExpiryField("7"),
	}}
}

// spannerBackupActions are a backup's page's actions.
func spannerBackupActions() []console.Action {
	return []console.Action{
		{ID: "updatebackup", Label: "Update expiry", Fields: []console.Field{spannerExpiryField("14")}},
		{ID: "copybackup", Label: "Copy backup", Fields: []console.Field{
			{Name: "backup", Label: "New backup ID", Type: "text", Required: true, Pattern: spannerIDPattern,
				Help: "The copy's ID, in this instance."},
			spannerExpiryField("7"),
		}},
		{ID: "restorebackup", Label: "Restore to a new database", Fields: []console.Field{
			{Name: "database", Label: "New database ID", Type: "text", Required: true, Pattern: spannerIDPattern,
				Help: "A database that does not exist yet in this instance: it is created with the backup's schema and rows."},
		}},
		spannerDeleteBackupAction(true),
	}
}

// spannerCreateConfigAction is Create custom configuration on a
// Google-managed configuration's page: the new one is based on it and copies
// its replicas. It is not on the instance's page, whose actions already fill
// a row (#1498).
func spannerCreateConfigAction() console.Action {
	return console.Action{ID: "createconfig", Label: "Create custom configuration", Fields: []console.Field{
		{Name: "config", Label: "Configuration ID", Type: "text", Required: true, Pattern: spannerCustomConfigPattern,
			Help: "Starts with custom-, as Spanner requires of a user-managed configuration. It is based on this configuration and copies its replicas."},
		{Name: "displayName", Label: "Display name", Type: "text", Help: "Optional; the ID when empty."},
	}}
}

// spannerMoveInstanceAction is Move instance, to one of the configurations.
func spannerMoveInstanceAction(configs []string, current string) console.Action {
	target := console.Field{Name: "targetConfig", Label: "Target configuration", Type: "text", Required: true,
		Help: "The instance configuration to move the instance to."}
	var others []string
	for _, c := range configs {
		if c != current {
			others = append(others, c)
		}
	}
	if len(others) > 0 {
		target.Type, target.Options, target.Default = "select", others, others[0]
	}
	return console.Action{ID: "moveinstance", Label: "Move instance", Fields: []console.Field{target},
		Confirm: "the instance's configuration: MoveInstance moves it, and every database in it, to the target configuration"}
}

// spannerInstanceExtraActions are the #1498 actions of an instance's page.
func (p spannerProvider) spannerInstanceExtraActions(ctx context.Context, project, instanceID string) []console.Action {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var databases, configs []string
	current := ""
	// The instance is read first: when it cannot be, the forms are offered
	// with text fields rather than waiting on listings that would fail too.
	if instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...); err == nil {
		inst, err := instAdmin.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: fmt.Sprintf("projects/%s/instances/%s", project, instanceID)})
		if err == nil {
			current = lastSegment(inst.GetConfig())
			it := instAdmin.ListInstanceConfigs(ctx, &instancepb.ListInstanceConfigsRequest{Parent: "projects/" + project})
			for {
				c, err := it.Next()
				if err != nil {
					break
				}
				configs = append(configs, lastSegment(c.GetName()))
			}
		}
		instAdmin.Close()
	}
	if current != "" {
		if dbAdmin, err := database.NewDatabaseAdminClient(ctx, localOpts(p.endpoint)...); err == nil {
			it := dbAdmin.ListDatabases(ctx, &databasepb.ListDatabasesRequest{Parent: fmt.Sprintf("projects/%s/instances/%s", project, instanceID)})
			for {
				db, err := it.Next()
				if err != nil {
					break
				}
				databases = append(databases, lastSegment(db.GetName()))
			}
			dbAdmin.Close()
		}
	}
	sort.Strings(databases)
	sort.Strings(configs)
	return []console.Action{spannerCreateBackupAction(databases), spannerMoveInstanceAction(configs, current)}
}

// spannerConfigDetail is one configuration's page, opened from the
// Instance configurations tab.
func (p spannerProvider) spannerConfigDetail(ctx context.Context, project, instanceID, configID string) (console.Detail, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return console.Detail{Unavailable: "cannot reach Spanner: " + err.Error()}, nil
	}
	defer instAdmin.Close()
	c, err := instAdmin.GetInstanceConfig(ctx, &instancepb.GetInstanceConfigRequest{Name: spannerConfigName(project, configID)})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the instance configuration: " + err.Error()}, nil
	}
	replicas := console.Listing{Columns: []string{"Type", "Default leader location"}, NameColumn: "Location", Noun: "replicas"}
	for _, r := range c.GetReplicas() {
		replicas.Items = append(replicas.Items, console.Resource{Name: r.GetLocation(),
			Fields: map[string]string{"Type": r.GetType().String(), "Default leader location": strconv.FormatBool(r.GetDefaultLeaderLocation())}})
	}
	replicas.Total = len(replicas.Items)
	return console.Detail{
		Summary: []console.Property{
			{Label: "Type", Value: c.GetConfigType().String()},
			{Label: "State", Value: c.GetState().String()},
			{Label: "Replicas", Value: fmt.Sprint(len(c.GetReplicas()))},
			{Label: "Viewed from instance", Value: instanceID},
		},
		Sections: []console.Section{
			{ID: "properties", Label: "Properties", Kind: console.KindProperties, Groups: []console.PropertyGroup{{
				Heading: "Instance configuration",
				Properties: []console.Property{
					{Label: "Resource name", Value: c.GetName()},
					{Label: "Display name", Value: c.GetDisplayName()},
					{Label: "Configuration type", Value: c.GetConfigType().String()},
					{Label: "Base configuration", Value: orDash(c.GetBaseConfig())},
					{Label: "Leader options", Value: strings.Join(c.GetLeaderOptions(), ", ")},
					{Label: "State", Value: c.GetState().String()},
				},
			}}},
			{ID: "replicas", Label: "Replicas", Listing: replicas},
		},
	}, nil
}

// spannerConfigActions are a configuration's page's: a custom one is renamed
// and deleted; a Google-managed one is the base of Create custom
// configuration.
func (p spannerProvider) spannerConfigActions(ctx context.Context, project, configID string) []console.Action {
	if !spannerCustomConfigRe.MatchString(configID) {
		return []console.Action{spannerCreateConfigAction()}
	}
	display := configID
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	if instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...); err == nil {
		if c, err := instAdmin.GetInstanceConfig(ctx, &instancepb.GetInstanceConfigRequest{Name: spannerConfigName(project, configID)}); err == nil {
			display = c.GetDisplayName()
		}
		instAdmin.Close()
	}
	return []console.Action{
		{ID: "renameconfig", Label: "Rename configuration", Fields: []console.Field{
			{Name: "displayName", Label: "Display name", Type: "text", Required: true, Default: display}}},
		{ID: "deleteconfig", Label: "Delete configuration", Destructive: true, Leaves: true},
	}
}

// spannerBackupsPage is [instance, "Backups"]: the breadcrumb between an
// instance and one of its backups.
func (p spannerProvider) spannerBackupsPage(ctx context.Context, project, instanceID string) (console.Detail, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	dbAdmin, err := database.NewDatabaseAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return console.Detail{Unavailable: "cannot reach Spanner: " + err.Error()}, nil
	}
	defer dbAdmin.Close()
	backups := spannerBackupListing(ctx, dbAdmin, instanceID, fmt.Sprintf("projects/%s/instances/%s", project, instanceID))
	return console.Detail{
		Summary:  []console.Property{{Label: "Instance", Value: instanceID}, {Label: "Backups", Value: fmt.Sprint(backups.Total)}},
		Sections: []console.Section{{ID: "backups", Label: "Backups", Listing: backups}},
	}, nil
}

// spannerBackupDetail is one backup's page, read with GetBackup.
func (p spannerProvider) spannerBackupDetail(ctx context.Context, project, instanceID, backupID string) (console.Detail, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	dbAdmin, err := database.NewDatabaseAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return console.Detail{Unavailable: "cannot reach Spanner: " + err.Error()}, nil
	}
	defer dbAdmin.Close()
	b, err := dbAdmin.GetBackup(ctx, &databasepb.GetBackupRequest{Name: spannerBackupName(project, instanceID, backupID)})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the backup: " + err.Error()}, nil
	}
	var restored []string
	for _, d := range b.GetReferencingDatabases() {
		restored = append(restored, lastSegment(d))
	}
	return console.Detail{
		Summary: []console.Property{
			{Label: "State", Value: b.GetState().String()},
			{Label: "Database", Value: orDash(lastSegment(b.GetDatabase()))},
			{Label: "Expire time", Value: spannerTS(b.GetExpireTime())},
			{Label: "Size", Value: spannerSize(b.GetSizeBytes())},
		},
		Sections: []console.Section{{
			ID: "properties", Label: "Properties", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{
				Heading: "Backup",
				Properties: []console.Property{
					{Label: "Resource name", Value: b.GetName()},
					{Label: "Database", Value: b.GetDatabase()},
					{Label: "Dialect", Value: b.GetDatabaseDialect().String()},
					{Label: "Version time", Value: spannerTS(b.GetVersionTime())},
					{Label: "Created", Value: spannerTS(b.GetCreateTime())},
					{Label: "Expire time", Value: spannerTS(b.GetExpireTime())},
					{Label: "Size", Value: spannerSize(b.GetSizeBytes())},
					{Label: "State", Value: b.GetState().String()},
					{Label: "Restoring databases", Value: orDash(strings.Join(restored, ", "))},
				},
			}},
		}},
	}, nil
}

// spannerDatabaseBackupSections are a database page's Backup schedules and
// Database roles tabs.
func spannerDatabaseBackupSections(ctx context.Context, dbAdmin *database.DatabaseAdminClient, instanceID, dbID, dbName string) []console.Section {
	schedules := console.Listing{Columns: []string{"Cron", "Retention", "Type", "Updated"}, NameColumn: "Backup schedule",
		Noun: "backup schedules"}
	sit := dbAdmin.ListBackupSchedules(ctx, &databasepb.ListBackupSchedulesRequest{Parent: dbName})
	for {
		s, err := sit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			schedules.Unavailable = "listing backup schedules: " + err.Error()
			break
		}
		id := lastSegment(s.GetName())
		cron := s.GetSpec().GetCronSpec().GetText()
		days := s.GetRetentionDuration().AsDuration().Hours() / 24
		kind := "Full"
		if s.GetIncrementalBackupSpec() != nil {
			kind = "Incremental"
		}
		schedules.Items = append(schedules.Items, console.Resource{
			Name: id,
			Fields: map[string]string{"Cron": orDash(cron), "Retention": strconv.FormatFloat(days, 'f', -1, 64) + " days", "Type": kind,
				"Updated": spannerTS(s.GetUpdateTime())},
			ActsOn: []string{instanceID, dbID, "backupSchedules", id},
			Actions: []console.Action{
				{ID: "editschedule", Label: "Edit backup schedule", Fields: []console.Field{
					{Name: "cron", Label: "Cron schedule", Type: "text", Required: true, Default: cron,
						Help: "When to take a backup, in UTC: five crontab fields, such as 0 2 * * *."},
					{Name: "retentionDays", Label: "Retention (days)", Type: "number", Required: true,
						Default: strconv.FormatFloat(days, 'f', -1, 64), Help: "How long each backup is kept: at most 366 days."},
				}},
				{ID: "deleteschedule", Label: "Delete backup schedule", Destructive: true},
			},
		})
	}
	sort.SliceStable(schedules.Items, func(i, j int) bool { return schedules.Items[i].Name < schedules.Items[j].Name })
	schedules.Total = len(schedules.Items)

	roles := console.Listing{NameColumn: "Database role", Noun: "database roles"}
	rit := dbAdmin.ListDatabaseRoles(ctx, &databasepb.ListDatabaseRolesRequest{Parent: dbName})
	for {
		r, err := rit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			roles.Unavailable = "listing database roles: " + err.Error()
			break
		}
		roles.Items = append(roles.Items, console.Resource{Name: lastSegment(r.GetName())})
	}
	roles.Total = len(roles.Items)

	return []console.Section{
		{ID: "backup-schedules", Label: "Backup schedules", Listing: schedules,
			Note: "Each schedule takes a full backup of this database when its cron fires, kept for its retention."},
		{ID: "database-roles", Label: "Database roles", Listing: roles,
			Note: "The roles fine-grained access control grants: the system roles, and those CREATE ROLE made (Update schema)."},
	}
}

// spannerCreateScheduleAction is a database page's Create backup schedule.
func spannerCreateScheduleAction() console.Action {
	return console.Action{ID: "createschedule", Label: "Create backup schedule", Fields: []console.Field{
		{Name: "schedule", Label: "Backup schedule ID", Type: "text", Required: true, Pattern: `^[a-z][a-z0-9_\-]{0,58}[a-z0-9]$`,
			Help: "Lowercase letters, digits, underscores and hyphens, starting with a letter."},
		{Name: "cron", Label: "Cron schedule", Type: "text", Required: true, Default: "0 2 * * *",
			Help: "When to take a backup, in UTC: five crontab fields, such as 0 2 * * * (daily at 02:00)."},
		{Name: "retentionDays", Label: "Retention (days)", Type: "number", Required: true, Default: "7",
			Help: "How long each backup is kept: at most 366 days. Each is a full backup."},
	}}
}

// spannerEditPartitionAction is Edit instance partition, prefilled.
func (p spannerProvider) spannerEditPartitionAction(ctx context.Context, project, instanceID, partitionID string) (console.Action, bool) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return console.Action{}, false
	}
	defer instAdmin.Close()
	ip, err := instAdmin.GetInstancePartition(ctx, &instancepb.GetInstancePartitionRequest{Name: spannerPartitionName(project, instanceID, partitionID)})
	if err != nil {
		return console.Action{}, false
	}
	return console.Action{ID: "editpartition", Label: "Edit instance partition", Fields: []console.Field{
		{Name: "displayName", Label: "Display name", Type: "text", Required: true, Default: ip.GetDisplayName(), Pattern: `^.{4,30}$`,
			Help: "4 to 30 characters."},
		{Name: "nodeCount", Label: "Nodes", Type: "number", Required: true, Default: fmt.Sprint(ip.GetNodeCount()),
			Help: "At least 1. The emulator's one process serves every partition."},
	}}, true
}

// spannerBackupAct performs the #1498 actions; handled is false for any other.
func (p spannerProvider) spannerBackupAct(ctx context.Context, project string, path []string, action string,
	values map[string]string) (handled bool, err error) {
	dbActs := map[string]bool{"createbackup": true, "deletebackup": true, "updatebackup": true, "copybackup": true,
		"restorebackup": true, "createschedule": true, "editschedule": true, "deleteschedule": true}
	instActs := map[string]bool{"createconfig": true, "renameconfig": true, "deleteconfig": true, "moveinstance": true, "editpartition": true}
	if !dbActs[action] && !instActs[action] {
		return false, nil
	}
	isBackup := len(path) == 3 && path[1] == spannerBackups
	isConfig := len(path) == 3 && path[1] == spannerConfigs
	isSchedule := len(path) == 4 && !isSpannerChildPath(path) && path[2] == "backupSchedules"
	isDatabase := len(path) == 2 && !isSpannerChildPath(path)
	where := map[string]struct {
		ok   bool
		what string
	}{
		"createbackup": {len(path) == 1, "an instance's"},
		"createconfig": {isConfig && !spannerCustomConfigRe.MatchString(path[2]), "a Google-managed instance configuration's"},
		"moveinstance": {len(path) == 1, "an instance's"},
		"deletebackup": {isBackup, "a backup's"}, "updatebackup": {isBackup, "a backup's"},
		"copybackup": {isBackup, "a backup's"}, "restorebackup": {isBackup, "a backup's"},
		"createschedule": {isDatabase, "a database's"},
		"editschedule":   {isSchedule, "a backup schedule's"}, "deleteschedule": {isSchedule, "a backup schedule's"},
		"renameconfig": {isConfig, "an instance configuration's"}, "deleteconfig": {isConfig, "an instance configuration's"},
		"editpartition": {len(path) == 3 && path[1] == spannerPartitions, "an instance partition's"},
	}[action]
	if !where.ok {
		return true, fmt.Errorf("%s is %s action", action, where.what)
	}
	instName := fmt.Sprintf("projects/%s/instances/%s", project, path[0])
	if dbActs[action] {
		dbAdmin, err := database.NewDatabaseAdminClient(ctx, localOpts(p.endpoint)...)
		if err != nil {
			return true, fmt.Errorf("cannot reach Spanner: %w", err)
		}
		defer dbAdmin.Close()
		return true, spannerDatabaseAct(ctx, dbAdmin, project, instName, path, action, values)
	}
	instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return true, fmt.Errorf("cannot reach Spanner: %w", err)
	}
	defer instAdmin.Close()
	return true, spannerInstanceAct(ctx, instAdmin, project, instName, path, action, values)
}

func spannerDatabaseAct(ctx context.Context, dbAdmin *database.DatabaseAdminClient, project, instName string, path []string,
	action string, values map[string]string) error {
	val := func(k string) string { return strings.TrimSpace(values[k]) }
	switch action {
	case "createbackup":
		db, id := val("database"), val("backup")
		if db == "" || id == "" {
			return errors.New("a database and a backup ID are required")
		}
		expire, err := spannerExpiry(values["expireDays"], time.Now())
		if err != nil {
			return err
		}
		op, err := dbAdmin.CreateBackup(ctx, &databasepb.CreateBackupRequest{Parent: instName, BackupId: id,
			Backup: &databasepb.Backup{Database: instName + "/databases/" + db, ExpireTime: expire}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	case "deletebackup":
		return dbAdmin.DeleteBackup(ctx, &databasepb.DeleteBackupRequest{Name: spannerBackupName(project, path[0], path[2])})
	case "updatebackup":
		expire, err := spannerExpiry(values["expireDays"], time.Now())
		if err != nil {
			return err
		}
		_, err = dbAdmin.UpdateBackup(ctx, &databasepb.UpdateBackupRequest{
			Backup:     &databasepb.Backup{Name: spannerBackupName(project, path[0], path[2]), ExpireTime: expire},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expire_time"}}})
		return err
	case "copybackup":
		id := val("backup")
		if id == "" {
			return errors.New("a new backup ID is required")
		}
		expire, err := spannerExpiry(values["expireDays"], time.Now())
		if err != nil {
			return err
		}
		op, err := dbAdmin.CopyBackup(ctx, &databasepb.CopyBackupRequest{Parent: instName, BackupId: id,
			SourceBackup: spannerBackupName(project, path[0], path[2]), ExpireTime: expire})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	case "restorebackup":
		id := val("database")
		if id == "" {
			return errors.New("a new database ID is required")
		}
		op, err := dbAdmin.RestoreDatabase(ctx, &databasepb.RestoreDatabaseRequest{Parent: instName, DatabaseId: id,
			Source: &databasepb.RestoreDatabaseRequest_Backup{Backup: spannerBackupName(project, path[0], path[2])}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	case "createschedule", "editschedule":
		cron := val("cron")
		if len(strings.Fields(cron)) != 5 {
			return fmt.Errorf("the cron schedule %q is not five crontab fields, such as 0 2 * * *", cron)
		}
		retention, err := spannerRetention(values["retentionDays"])
		if err != nil {
			return err
		}
		dbName := instName + "/databases/" + path[1]
		s := &databasepb.BackupSchedule{
			Spec:              &databasepb.BackupScheduleSpec{ScheduleSpec: &databasepb.BackupScheduleSpec_CronSpec{CronSpec: &databasepb.CrontabSpec{Text: cron}}},
			RetentionDuration: retention,
		}
		if action == "editschedule" {
			s.Name = dbName + "/backupSchedules/" + path[3]
			_, err := dbAdmin.UpdateBackupSchedule(ctx, &databasepb.UpdateBackupScheduleRequest{BackupSchedule: s,
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.cron_spec.text", "retention_duration"}}})
			return err
		}
		id := val("schedule")
		if id == "" {
			return errors.New("a backup schedule ID is required")
		}
		s.BackupTypeSpec = &databasepb.BackupSchedule_FullBackupSpec{FullBackupSpec: &databasepb.FullBackupSpec{}}
		_, err = dbAdmin.CreateBackupSchedule(ctx, &databasepb.CreateBackupScheduleRequest{Parent: dbName, BackupScheduleId: id, BackupSchedule: s})
		return err
	case "deleteschedule":
		return dbAdmin.DeleteBackupSchedule(ctx, &databasepb.DeleteBackupScheduleRequest{
			Name: instName + "/databases/" + path[1] + "/backupSchedules/" + path[3]})
	}
	return fmt.Errorf("unknown action %q", action)
}

func spannerInstanceAct(ctx context.Context, instAdmin *instance.InstanceAdminClient, project, instName string, path []string,
	action string, values map[string]string) error {
	val := func(k string) string { return strings.TrimSpace(values[k]) }
	switch action {
	case "createconfig":
		id, base := val("config"), path[2]
		if !spannerCustomConfigRe.MatchString(id) {
			return fmt.Errorf("a custom configuration's ID starts with custom-, not %q", id)
		}
		b, err := instAdmin.GetInstanceConfig(ctx, &instancepb.GetInstanceConfigRequest{Name: spannerConfigName(project, base)})
		if err != nil {
			return fmt.Errorf("reading the base configuration: %w", err)
		}
		display := val("displayName")
		if display == "" {
			display = id
		}
		op, err := instAdmin.CreateInstanceConfig(ctx, &instancepb.CreateInstanceConfigRequest{
			Parent: "projects/" + project, InstanceConfigId: id,
			InstanceConfig: &instancepb.InstanceConfig{Name: spannerConfigName(project, id), DisplayName: display,
				BaseConfig: b.GetName(), Replicas: b.GetReplicas()}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	case "renameconfig":
		display := val("displayName")
		if display == "" {
			return errors.New("a display name is required")
		}
		op, err := instAdmin.UpdateInstanceConfig(ctx, &instancepb.UpdateInstanceConfigRequest{
			InstanceConfig: &instancepb.InstanceConfig{Name: spannerConfigName(project, path[2]), DisplayName: display},
			UpdateMask:     &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	case "deleteconfig":
		return instAdmin.DeleteInstanceConfig(ctx, &instancepb.DeleteInstanceConfigRequest{Name: spannerConfigName(project, path[2])})
	case "moveinstance":
		target := val("targetConfig")
		if target == "" {
			return errors.New("a target configuration is required")
		}
		op, err := instAdmin.MoveInstance(ctx, &instancepb.MoveInstanceRequest{Name: instName, TargetConfig: spannerConfigName(project, target)})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	case "editpartition":
		nodes, err := strconv.ParseInt(val("nodeCount"), 10, 32)
		if err != nil || nodes < 1 {
			return fmt.Errorf("nodes must be a whole number of at least 1, not %q", values["nodeCount"])
		}
		display := val("displayName")
		if n := len([]rune(display)); n < 4 || n > 30 {
			return errors.New("the display name must be 4 to 30 characters long")
		}
		op, err := instAdmin.UpdateInstancePartition(ctx, &instancepb.UpdateInstancePartitionRequest{
			InstancePartition: &instancepb.InstancePartition{Name: spannerPartitionName(project, path[0], path[2]), DisplayName: display,
				ComputeCapacity: &instancepb.InstancePartition_NodeCount{NodeCount: int32(nodes)}},
			FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "node_count"}}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}
