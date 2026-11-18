package bigqueryfront

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Region INFORMATION_SCHEMA.JOBS_BY_FOLDER and JOBS_BY_ORGANIZATION
// (#1392), answered as region JOBS is (infoschemajobs.go).
//
// "JOBS_BY_FOLDER: Jobs submitted in the parent folder of the current
// project, including the jobs in folders under it"; "JOBS_BY_ORGANIZATION:
// Jobs submitted in the organization that is associated with the current
// project" (https://cloud.google.com/bigquery/docs/information-schema-jobs-by-folder,
// -jobs-by-organization). The project's folders and organization are the
// instance's Resource Manager's (#1294), read with v1 projects.getAncestry
// for each query, through the cloudburrow-host Service (the front is given
// its address with --resourcemanager). The rows are the JOBS rows of
// every ACTIVE project of Resource Manager under the folder (in it, or in
// a folder under it) or the organization: the query's own and, since
// #1767, the other projects' BigQuery serves (#1764), each read as its own
// project's JOBS is and named by project_id. JOBS_BY_FOLDER adds
// folder_numbers, "Number IDs of folders that contain the project,
// starting with the folder that immediately contains the project";
// JOBS_BY_ORGANIZATION has JOBS' columns.
//
// A project in no folder (JOBS_BY_FOLDER) or no organization
// (JOBS_BY_ORGANIZATION) is refused 400 (wording UNVERIFIED); with no
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/information-schema-jobs-by-organization
// Resource Manager to ask, the views are 501. Regions are as for JOBS: the
// instance's jobs are in US.

// hierarchyJobsViews are the views read through the project's ancestry.
var hierarchyJobsViews = map[string]bool{"JOBS_BY_FOLDER": true, "JOBS_BY_ORGANIZATION": true}

// ancestor is one step of a project's ancestry, as getAncestry gives it.
type ancestor struct {
	ResourceID struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"resourceId"`
}

// ancestry reads the project's ancestry, nearest first, from Resource
// Manager; a project Resource Manager does not have has none above it.
func (f front) ancestry(ctx context.Context, project string) ([]ancestor, int, string) {
	if f.resourceManager == "" {
		return nil, http.StatusNotImplemented, "Not implemented here: this view reads the project's folders and organization " +
			"from Resource Manager, which the BigQuery front was not given, so nothing was run."
	}
	u := strings.TrimRight(f.resourceManager, "/") + "/v1/projects/" + url.PathEscape(project) + ":getAncestry"
	// Bounded, sent again when it fails, logged when slow (serviceclient.go,
	// #1628).
	status, body, err := serviceCall(ctx, "Resource Manager projects.getAncestry of "+project, http.MethodPost, u,
		"application/json", []byte("{}"))
	if err != nil {
		return nil, http.StatusInternalServerError, "cloudburrow: could not read the project's ancestry from Resource Manager: " + err.Error()
	}
	if status == http.StatusNotFound || status == http.StatusForbidden {
		return nil, 0, ""
	}
	var out struct {
		Ancestor []ancestor `json:"ancestor"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &out) != nil {
		return nil, http.StatusInternalServerError, "cloudburrow: could not read the project's ancestry from Resource Manager: " +
			errorMessage(body, status)
	}
	return out.Ancestor, 0, ""
}

// hierarchyJobsRows returns the subquery of view's rows: the project's
// jobs, when it is in a folder (JOBS_BY_FOLDER) or an organization
// (JOBS_BY_ORGANIZATION).
func (f front) hierarchyJobsRows(r *http.Request, view string, filter *jobsFilter) (string, int, string) {
	project := projectOf(f.base)
	chain, st, msg := f.ancestry(r.Context(), project)
	if st != 0 {
		return "", st, msg
	}
	var folders []string
	org := ""
	for _, a := range chain {
		switch a.ResourceID.Type {
		case "folder":
			folders = append(folders, a.ResourceID.ID)
		case "organization":
			org = a.ResourceID.ID
		}
	}
	// The folder that immediately holds the project is the step after
	// it.
	inFolder := len(chain) > 1 && chain[1].ResourceID.Type == "folder"
	if view == "JOBS_BY_FOLDER" && !inFolder {
		return "", http.StatusBadRequest, fmt.Sprintf("Project %s is not in a folder: INFORMATION_SCHEMA.JOBS_BY_FOLDER "+
			"reads the jobs of the folder that holds the project", project)
	}
	if view == "JOBS_BY_ORGANIZATION" && org == "" {
		return "", http.StatusBadRequest, fmt.Sprintf("Project %s is not in an organization: "+
			"INFORMATION_SCHEMA.JOBS_BY_ORGANIZATION reads the jobs of the project's organization", project)
	}
	// The scope: the folder that holds the project, or its organization.
	scope := ancestor{}
	if view == "JOBS_BY_FOLDER" {
		scope = chain[1]
	} else {
		scope.ResourceID.Type, scope.ResourceID.ID = "organization", org
	}
	sub, st, msg := f.jobsRows(r, filter)
	if st != 0 {
		return "", st, msg
	}
	parts := []string{withFolders(view, sub, folders)}
	others, st, msg := f.activeProjects(r.Context())
	if st != 0 {
		return "", st, msg
	}
	for _, other := range others {
		if other == project {
			continue
		}
		oc, st, msg := f.ancestry(r.Context(), other)
		if st != 0 {
			return "", st, msg
		}
		var ofolders []string
		in := false
		for i, a := range oc {
			if i > 0 && a.ResourceID.Type == "folder" {
				ofolders = append(ofolders, a.ResourceID.ID)
			}
			in = in || i > 0 && a.ResourceID == scope.ResourceID
		}
		if !in {
			continue
		}
		osub, st, msg := f.inProject(other).jobsRows(r, filter)
		if st != 0 {
			return "", st, msg
		}
		// Its table of JOBS rows is the other project's (jobstable.go).
		osub = strings.ReplaceAll(osub, jobsTablePath(), quoteName(other)+"."+jobsTablePath())
		parts = append(parts, withFolders(view, osub, ofolders))
	}
	if len(parts) == 1 {
		return parts[0], 0, ""
	}
	for i, p := range parts {
		parts[i] = "SELECT * FROM " + p
	}
	return "(" + strings.Join(parts, " UNION ALL ") + ")", 0, ""
}

// withFolders is sub with JOBS_BY_FOLDER's folder_numbers, the project's
// folders nearest first; sub itself for JOBS_BY_ORGANIZATION.
func withFolders(view, sub string, folders []string) string {
	if view != "JOBS_BY_FOLDER" {
		return sub
	}
	return "(SELECT *, ARRAY<INT64>[" + strings.Join(folders, ", ") + "] AS folder_numbers FROM " + sub + ")"
}

// activeProjects lists the ACTIVE projects of Resource Manager (v3
// projects.search, every page), by ID.
func (f front) activeProjects(ctx context.Context) ([]string, int, string) {
	var out []string
	token := ""
	for {
		u := strings.TrimRight(f.resourceManager, "/") + "/v3/projects:search?pageSize=1000"
		if token != "" {
			u += "&pageToken=" + url.QueryEscape(token)
		}
		status, body, err := serviceCall(ctx, "Resource Manager projects.search", http.MethodGet, u, "", nil)
		var page struct {
			Projects []struct {
				ProjectID string `json:"projectId"`
				State     string `json:"state"`
			} `json:"projects"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err != nil {
			return nil, http.StatusInternalServerError, "cloudburrow: could not list the projects of Resource Manager: " + err.Error()
		}
		if status != http.StatusOK || json.Unmarshal(body, &page) != nil {
			return nil, http.StatusInternalServerError, "cloudburrow: could not list the projects of Resource Manager: " +
				errorMessage(body, status)
		}
		for _, p := range page.Projects {
			if p.State == "ACTIVE" {
				out = append(out, p.ProjectID)
			}
		}
		if token = page.NextPageToken; token == "" {
			return out, 0, ""
		}
	}
}
