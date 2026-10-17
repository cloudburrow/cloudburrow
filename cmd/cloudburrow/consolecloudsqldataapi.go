package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	sqladminapi "google.golang.org/api/sqladmin/v1"
)

// The Cloud SQL page's actions for #1462: Run SQL through the Data API
// (instances.executeSql), Pre-check upgrade (preCheckMajorVersionUpgrade),
// Truncate log (truncateLog, MySQL), Delete with final backup
// (instances.delete with enableFinalBackup), and the project's backups
// through the Backups API: Edit, Restore into this instance and Delete,
// which reach a FINAL backup whose instance is gone.

// projectBackupNames are the project's SUCCESSFUL backups' names, through
// ListBackups, with each one's instance and type.
func projectBackupNames(ctx context.Context, svc *sqladminapi.Service, project string) ([]string, []string) {
	list, err := svc.Backups.ListBackups("projects/" + project).Context(ctx).Do()
	if err != nil {
		return nil, nil
	}
	var names, labels []string
	for _, b := range list.Backups {
		if b.State != "SUCCESSFUL" {
			continue
		}
		names = append(names, b.Name)
		labels = append(labels, b.Instance+" "+b.Type)
	}
	return names, labels
}

// projectBackupsListing is the Project backups tab: the project's backups
// of instance, through ListBackups.
func projectBackupsListing(ctx context.Context, svc *sqladminapi.Service, project, instance string) console.Listing {
	l := console.Listing{Columns: []string{"Type", "Description", "Created", "Expires"}, NameColumn: "Backup", Noun: "project backups",
		Note: "The project's backups of this instance (the Backups API): Create project backup takes one; a FINAL backup outlives its instance."}
	list, err := svc.Backups.ListBackups("projects/" + project).Filter(`instance="` + instance + `"`).Context(ctx).Do()
	if err != nil {
		l.Unavailable = sqlAPIError(err).Error()
		return l
	}
	for _, b := range list.Backups {
		created := ""
		if b.BackupInterval != nil {
			created = b.BackupInterval.StartTime
		}
		l.Items = append(l.Items, console.Resource{Name: lastSegment(b.Name), Status: b.State, Fields: map[string]string{
			"Type": b.Type, "Description": b.Description, "Created": created, "Expires": b.ExpiryTime}})
	}
	l.Total = len(l.Items)
	return l
}

// dataAPIActions are the #1462 actions an instance's page offers.
func dataAPIActions(ctx context.Context, svc *sqladminapi.Service, project string, in *sqladminapi.DatabaseInstance) []console.Action {
	var acts []console.Action
	created := !upSQLInstance(in.Name)
	if created && in.Settings != nil && in.Settings.DataApiAccess == "ALLOW_DATA_API" {
		acts = append(acts, console.Action{ID: "run-sql", Label: "Run SQL (Data API)", Fields: []console.Field{
			{Name: "database", Label: "Database", Type: "text", Required: true},
			{Name: "user", Label: "Database user", Type: "text", Required: true},
			{Name: "passwordSecretVersion", Label: "Password secret version", Type: "text", Required: true,
				Help: "A Secret Manager secret version holding the user's password: projects/{project}/secrets/{secret}/versions/{version}."},
			{Name: "sql", Label: "SQL", Type: "textarea", Required: true},
			{Name: "rowLimit", Label: "Row limit", Type: "number", Default: "1000",
				Help: "At most this many rows of each result are shown; a longer result is returned partial."}}})
	}
	var opts []string
	for _, v := range in.UpgradableDatabaseVersions {
		opts = append(opts, v.MajorVersion)
	}
	if created && len(opts) > 0 {
		acts = append(acts, console.Action{ID: "precheck-upgrade", Label: "Pre-check upgrade", Fields: []console.Field{
			{Name: "databaseVersion", Label: "Target version", Type: "select", Required: true, Options: opts, Default: opts[len(opts)-1],
				Help: "Checks the upgrade as Upgrade would make it, and lists what would stop it."}}})
	}
	if strings.HasPrefix(in.DatabaseVersion, "MYSQL_") {
		acts = append(acts, console.Action{ID: "truncate-log", Label: "Truncate log", Destructive: true,
			Confirm: "The log table's rows are deleted.", Fields: []console.Field{{Name: "logType", Label: "Log table", Type: "select",
				Required: true, Options: []string{"MYSQL_GENERAL_TABLE", "MYSQL_SLOW_TABLE"}, Default: "MYSQL_GENERAL_TABLE"}}})
	}
	if created {
		acts = append(acts, console.Action{ID: "delete-final-backup", Label: "Delete with final backup", Destructive: true, Leaves: true,
			Confirm: "The instance is deleted after a FINAL backup of it is taken; the backup is kept until it expires.",
			Fields: []console.Field{{Name: "description", Label: "Final backup description", Type: "text"},
				{Name: "ttlDays", Label: "Keep for (days)", Type: "number", Default: "30"}}})
	}
	if in.State == "RUNNABLE" {
		acts = append(acts, console.Action{ID: "create-project-backup", Label: "Create project backup", Fields: []console.Field{
			{Name: "description", Label: "Description", Type: "text"},
			{Name: "ttlDays", Label: "Keep for (days)", Type: "number",
				Help: "Empty keeps it until it is deleted. The backup is the project's (Backups API), listed in Project backups."}}})
	}
	if names, _ := projectBackupNames(ctx, svc, project); len(names) > 0 {
		acts = append(acts,
			console.Action{ID: "edit-project-backup", Label: "Edit project backup", Fields: []console.Field{
				{Name: "backup", Label: "Backup", Type: "select", Required: true, Options: names, Default: names[0]},
				{Name: "description", Label: "Description", Type: "text", Help: "Empty keeps it."},
				{Name: "ttlDays", Label: "Keep for (days)", Type: "number",
					Help: "A FINAL backup's retention, from now; leave empty to keep it."}}},
			console.Action{ID: "restore-project-backup", Label: "Restore project backup", Destructive: true,
				Confirm: "Restoring replaces every database of the instance with the backup's; what was written since is lost.",
				Fields:  []console.Field{{Name: "backup", Label: "Backup", Type: "select", Required: true, Options: names, Default: names[0]}}},
			console.Action{ID: "delete-project-backup", Label: "Delete project backup", Destructive: true, Confirm: "The backup's dump is deleted.",
				Fields: []console.Field{{Name: "backup", Label: "Backup", Type: "select", Required: true, Options: names, Default: names[0]}}})
	}
	return acts
}

// dataAPIResult performs Run SQL and Pre-check upgrade, which answer with
// a listing; ok is false for any other action.
func dataAPIResult(ctx context.Context, svc *sqladminapi.Service, project, name, action string, v map[string]string) (*console.Listing, bool, error) {
	switch action {
	case "run-sql":
		var limit int64
		if s := strings.TrimSpace(v["rowLimit"]); s != "" {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil || n < 0 {
				return nil, true, fmt.Errorf("row limit %q is not a number", s)
			}
			limit = n
		}
		res, err := svc.Instances.ExecuteSql(project, name, &sqladminapi.ExecuteSqlPayload{Database: strings.TrimSpace(v["database"]),
			User: strings.TrimSpace(v["user"]), PasswordSecretVersion: strings.TrimSpace(v["passwordSecretVersion"]),
			SqlStatement: v["sql"], RowLimit: limit, PartialResultMode: "ALLOW_PARTIAL_RESULT", Application: "cloudburrow-console"}).Context(ctx).Do()
		if err != nil {
			return nil, true, sqlAPIError(err)
		}
		if res.Status != nil && res.Status.Message != "" {
			return nil, true, fmt.Errorf("%s", res.Status.Message)
		}
		return sqlResultListing(res), true, nil
	case "precheck-upgrade":
		op, err := svc.Instances.PreCheckMajorVersionUpgrade(project, name, &sqladminapi.InstancesPreCheckMajorVersionUpgradeRequest{
			PreCheckMajorVersionUpgradeContext: &sqladminapi.PreCheckMajorVersionUpgradeContext{TargetDatabaseVersion: v["databaseVersion"]}}).Context(ctx).Do()
		if err != nil {
			return nil, true, sqlAPIError(err)
		}
		out := &console.Listing{Columns: []string{"Message", "Actions required"}, NameColumn: "Type", Noun: "findings"}
		if pc := op.PreCheckMajorVersionUpgradeContext; pc != nil {
			for _, r := range pc.PreCheckResponse {
				out.Items = append(out.Items, console.Resource{Name: r.MessageType, Fields: map[string]string{
					"Message": r.Message, "Actions required": strings.Join(r.ActionsRequired, "; ")}})
			}
		}
		if len(out.Items) == 0 {
			out.Items = append(out.Items, console.Resource{Name: "INFO", Fields: map[string]string{
				"Message": "Nothing stops the upgrade to " + v["databaseVersion"] + "."}})
		}
		out.Total = len(out.Items)
		return out, true, nil
	}
	return nil, false, nil
}

// sqlResultListing is executeSql's last result with columns, or the
// statements' messages when none has any.
func sqlResultListing(res *sqladminapi.SqlInstancesExecuteSqlResponse) *console.Listing {
	for i := len(res.Results) - 1; i >= 0; i-- {
		r := res.Results[i]
		if len(r.Columns) == 0 {
			continue
		}
		out := &console.Listing{NameColumn: "#", Noun: "rows"}
		for _, c := range r.Columns {
			out.Columns = append(out.Columns, c.Name)
		}
		for n, row := range r.Rows {
			f := map[string]string{}
			for j, val := range row.Values {
				if j < len(r.Columns) {
					if val.NullValue {
						f[r.Columns[j].Name] = "NULL"
					} else {
						f[r.Columns[j].Name] = val.Value
					}
				}
			}
			out.Items = append(out.Items, console.Resource{Name: strconv.Itoa(n + 1), Fields: f})
		}
		out.Total = len(out.Items)
		if r.PartialResult {
			out.Note = "The result is partial: only the first rows, up to the row limit, are shown."
		}
		return out
	}
	out := &console.Listing{Columns: []string{"Message"}, NameColumn: "Statement", Noun: "statements"}
	for i, r := range res.Results {
		out.Items = append(out.Items, console.Resource{Name: strconv.Itoa(i + 1), Fields: map[string]string{"Message": r.Message}})
	}
	out.Total = len(out.Items)
	return out
}

// dataAPIAct performs the other #1462 actions; ok is false for any other
// action.
func dataAPIAct(ctx context.Context, svc *sqladminapi.Service, project, name, action string, v map[string]string) (bool, error) {
	var err error
	switch action {
	case "truncate-log":
		_, err = svc.Instances.TruncateLog(project, name, &sqladminapi.InstancesTruncateLogRequest{
			TruncateLogContext: &sqladminapi.TruncateLogContext{LogType: v["logType"]}}).Context(ctx).Do()
	case "delete-final-backup":
		call := svc.Instances.Delete(project, name).EnableFinalBackup(true)
		if d := strings.TrimSpace(v["description"]); d != "" {
			call = call.FinalBackupDescription(d)
		}
		if s := strings.TrimSpace(v["ttlDays"]); s != "" {
			n, perr := strconv.ParseInt(s, 10, 64)
			if perr != nil {
				return true, fmt.Errorf("days %q is not a number", s)
			}
			call = call.FinalBackupTtlDays(n)
		}
		_, err = call.Context(ctx).Do()
	case "create-project-backup":
		b := &sqladminapi.Backup{Instance: name, Description: strings.TrimSpace(v["description"])}
		if s := strings.TrimSpace(v["ttlDays"]); s != "" {
			n, perr := strconv.ParseInt(s, 10, 64)
			if perr != nil || n <= 0 {
				return true, fmt.Errorf("days %q is not a number of days", s)
			}
			b.TtlDays = n
		}
		_, err = svc.Backups.CreateBackup("projects/"+project, b).Context(ctx).Do()
	case "edit-project-backup":
		// Only what the form was given is in the mask: an empty Description
		// keeps the backup's, where it used to clear it.
		b := &sqladminapi.Backup{Description: strings.TrimSpace(v["description"])}
		var mask []string
		if b.Description != "" {
			mask = append(mask, "description")
		}
		if s := strings.TrimSpace(v["ttlDays"]); s != "" {
			n, perr := strconv.ParseInt(s, 10, 64)
			if perr != nil {
				return true, fmt.Errorf("days %q is not a number", s)
			}
			b.TtlDays = n
			mask = append(mask, "ttl_days")
		}
		if len(mask) == 0 {
			return true, errors.New("nothing to change: set a description or a number of days")
		}
		_, err = svc.Backups.UpdateBackup(v["backup"], b).UpdateMask(strings.Join(mask, ",")).Context(ctx).Do()
	case "restore-project-backup":
		_, err = svc.Instances.RestoreBackup(project, name, &sqladminapi.InstancesRestoreBackupRequest{Backup: v["backup"]}).Context(ctx).Do()
	case "delete-project-backup":
		_, err = svc.Backups.DeleteBackup(v["backup"]).Context(ctx).Do()
	default:
		return false, nil
	}
	if err != nil {
		return true, sqlAPIError(err)
	}
	return true, nil
}
