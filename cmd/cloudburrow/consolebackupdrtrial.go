package main

// The Backup and DR page's own actions (#1536), on no vault: Find protected
// resources (resourceBackupConfigs.fetch with gcloud's filter and order, of
// the project, or of a folder or the organization: every project under it),
// Show trial, Subscribe to trial and End trial (getTrial, trial.subscribe,
// trial.end), and Check Assured Workloads (managementServers.msComplianceMetadata).

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

var bdrTrialLocations = []string{"us", "eu", "asia"}

// ListActions implements console.ListActor.
func (backupDRProvider) ListActions(_ context.Context, project string) []console.Action {
	if project == "" {
		return nil
	}
	trialLoc := console.Field{Name: "location", Label: "Location", Type: "select", Required: true, Options: bdrTrialLocations,
		Help: "A Backup and DR trial is per multi-region: us, eu or asia."}
	return []console.Action{
		{ID: "protected", Label: "Find protected resources", Fields: []console.Field{
			{Name: "location", Label: "Region", Type: "text", Required: true, Default: "us-central1", Help: "A region, or - for every region."},
			{Name: "filter", Label: "Filter", Type: "text",
				Help: `As gcloud backup-dr resource-backup-config list takes it: target_resource_display_name="db" AND vaulted=true; ` +
					"the keys are target_resource_display_name, target_resource_type, backup_configured, vaulted, " +
					"backup_configs_details.backup_config_source_display_name and backup_configs_details.type."},
			{Name: "order", Label: "Order by name", Type: "select", Options: []string{"ascending", "descending"}, Default: "ascending"},
			{Name: "scope", Label: "Folder or organization", Type: "text",
				Help: "Blank for this project; folders/{number} for every project under that folder and its subfolders, or " +
					resourcemanager.OrganizationName + " for every project under the organization."}}},
		{ID: "trial", Label: "Show trial", Fields: []console.Field{trialLoc}},
		{ID: "subscribetrial", Label: "Subscribe to trial", Confirm: "A project subscribes to a Backup and DR trial once; it lasts 30 days.",
			Fields: []console.Field{trialLoc}},
		{ID: "endtrial", Label: "End trial", Confirm: "An ended trial cannot be subscribed to again.", Fields: []console.Field{trialLoc,
			{Name: "reason", Label: "Reason", Type: "select", Required: true, Options: []string{"MOVE_TO_PAID", "DISCONTINUED"}}}},
		{ID: "compliance", Label: "Check Assured Workloads", Fields: []console.Field{
			{Name: "location", Label: "Region", Type: "text", Required: true, Default: "us-central1"}}},
	}
}

// ActAtResult implements console.ResultActor: the list screen's actions
// answer with rows; a vault's and a data source's go to ActAt.
func (p backupDRProvider) ActAtResult(ctx context.Context, project string, path []string, action string, v map[string]string) (*console.Listing, error) {
	if len(path) > 0 {
		return nil, p.ActAt(ctx, project, path, action, v)
	}
	if project == "" {
		return nil, errors.New("choose a project first")
	}
	loc := "/v1/projects/" + project + "/locations/" + url.PathEscape(v["location"])
	switch action {
	case "protected":
		q := url.Values{}
		if f := strings.TrimSpace(v["filter"]); f != "" {
			q.Set("filter", f)
		}
		if v["order"] == "descending" {
			q.Set("orderBy", "target_resource_display_name desc")
		}
		l := &console.Listing{Columns: []string{"Project", "Type", "Backup configured", "Vaulted", "Backup configs"}, NameColumn: "Resource",
			Noun: "protected resources", Note: "resourceBackupConfigs.fetch: each Cloud SQL instance of the region and how it is backed up."}
		parent := loc
		if scope := strings.Trim(strings.TrimSpace(v["scope"]), "/"); scope != "" {
			kind, id, _ := strings.Cut(scope, "/")
			if (kind != "folders" && kind != "organizations") || id == "" || strings.Contains(id, "/") {
				return nil, errors.New("the folder or organization is folders/{number} or organizations/{number}")
			}
			parent = "/v1/" + kind + "/" + url.PathEscape(id) + "/locations/" + url.PathEscape(v["location"])
			l.Note = "resourceBackupConfigs.fetch of " + scope + ": each Cloud SQL instance of the region, in every project under it, and how it is backed up."
		}
		for token := ""; ; {
			if token != "" {
				q.Set("pageToken", token)
			}
			out, err := p.call(ctx, p.bdr, http.MethodGet, parent+"/resourceBackupConfigs:fetch?"+q.Encode(), nil)
			if err != nil {
				return nil, err
			}
			for _, x := range bdrItems(out, "resourceBackupConfigs") {
				var cfgs []string
				for _, d := range bdrItems(x, "backupConfigsDetails") {
					cfgs = append(cfgs, str(d, "type")+" "+str(d, "backupConfigSourceDisplayName"))
				}
				proj, _, _ := strings.Cut(strings.TrimPrefix(str(x, "name"), "projects/"), "/")
				l.Items = append(l.Items, console.Resource{Name: str(x, "targetResourceDisplayName"), Fields: map[string]string{
					"Project": proj, "Type": str(x, "targetResourceType"), "Backup configured": boolText(x["backupConfigured"]),
					"Vaulted": boolText(x["vaulted"]), "Backup configs": strings.Join(cfgs, "; ")}})
			}
			if token = str(out, "nextPageToken"); token == "" {
				break
			}
		}
		l.Total = len(l.Items)
		return l, nil
	case "trial", "subscribetrial", "endtrial":
		var out map[string]any
		var err error
		switch action {
		case "trial":
			out, err = p.get(ctx, strings.TrimPrefix(loc, "/v1/")+"/trial")
		case "subscribetrial":
			out, err = p.call(ctx, p.bdr, http.MethodPost, loc+"/trial:subscribe", map[string]any{})
		default:
			out, err = p.call(ctx, p.bdr, http.MethodPost, loc+"/trial:end", map[string]any{"endReason": v["reason"]})
		}
		if err != nil {
			return nil, err
		}
		// The state is a column too: an action's result table shows the
		// columns, not a row's status (#1592).
		return &console.Listing{Columns: []string{"State", "Started", "Ends", "End reason"}, NameColumn: "Trial", Noun: "trials", Total: 1,
			Items: []console.Resource{{Name: str(out, "name"), Status: str(out, "state"), Fields: map[string]string{
				"State": str(out, "state"), "Started": str(out, "startTime"), "Ends": str(out, "endTime"), "End reason": str(out, "endReason")}}}}, nil
	case "compliance":
		out, err := p.call(ctx, p.bdr, http.MethodPost, loc+"/managementServers:msComplianceMetadata", map[string]any{"projectId": project})
		if err != nil {
			return nil, err
		}
		return &console.Listing{Columns: []string{"Assured Workloads"}, NameColumn: "Project", Noun: "projects", Total: 1,
			Note:  "managementServers.msComplianceMetadata: whether the project is an Assured Workloads project; CloudBurrow runs no Assured Workloads.",
			Items: []console.Resource{{Name: project, Fields: map[string]string{"Assured Workloads": boolText(out["isAssuredWorkload"])}}}}, nil
	}
	return nil, errors.New("unknown action " + action)
}

func boolText(v any) string {
	if b, _ := v.(bool); b {
		return "Yes"
	}
	return "No"
}
