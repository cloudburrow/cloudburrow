package bigqueryfront

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
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
// its address with --resourcemanager). The emulator serves one project,
// the instance's, so the jobs of every project under the folder or the
// organization are that project's: its JOBS rows. JOBS_BY_FOLDER adds
// folder_numbers, "Number IDs of folders that contain the project,
// starting with the folder that immediately contains the project";
// JOBS_BY_ORGANIZATION has JOBS' columns.
//
// A project in no folder (JOBS_BY_FOLDER) or no organization
// (JOBS_BY_ORGANIZATION) is refused 400 (wording UNVERIFIED); with no
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
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	u := strings.TrimRight(f.resourceManager, "/") + "/v1/projects/" + url.PathEscape(project) + ":getAncestry"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader("{}"))
	if err != nil {
		return nil, http.StatusInternalServerError, "cloudburrow: " + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, http.StatusInternalServerError, "cloudburrow: could not read the project's ancestry from Resource Manager: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		return nil, 0, ""
	}
	var out struct {
		Ancestor []ancestor `json:"ancestor"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &out) != nil {
		return nil, http.StatusInternalServerError, "cloudburrow: could not read the project's ancestry from Resource Manager: " +
			errorMessage(body, resp.StatusCode)
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
	sub, st, msg := f.jobsRows(r, filter)
	if st != 0 || view != "JOBS_BY_FOLDER" {
		return sub, st, msg
	}
	return "(SELECT *, ARRAY<INT64>[" + strings.Join(folders, ", ") + "] AS folder_numbers FROM " + sub + ")", 0, ""
}
