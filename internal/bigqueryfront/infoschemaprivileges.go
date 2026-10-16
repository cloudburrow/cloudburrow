package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Region INFORMATION_SCHEMA OBJECT_PRIVILEGES and SCHEMATA_LINKS (#1355),
// answered as the region forms of the dataset views are (regionViews,
// infoschemajobs.go), with BigQuery's documented columns
// (https://cloud.google.com/bigquery/docs/information-schema-object-privileges,
// -datasets-schemata-links). Measured first: the front refused both 501.
// Both are project views, region-qualified; a dataset-qualified reference
// is refused 400 invalidQuery, as BigQuery documents that a query of
// either "must include a region qualifier" (regionOnlyInfoSchema, #1458).
//
// OBJECT_PRIVILEGES has a row per role binding set on the object itself,
// none inherited: for a dataset (object_schema NULL, object_type SCHEMA)
// each entry of its access list that grants a role (a user, group, domain,
// special group or IAM member; an authorized view, routine or dataset
// grants none), its legacy roles READER, WRITER and OWNER written as
// roles/bigquery.dataViewer, dataEditor and dataOwner, and a special group
// as BigQuery's example writes it (projectOwner, projectEditor,
// projectViewer); for a table or view (object_type its tables.get type)
// each member of each binding of the IAM policy the front keeps (iam.go,
// #1231). BigQuery requires a WHERE clause that names object_name (and
// object_schema for a table); a query that names no object_name after a
// WHERE is refused 400 (wording UNVERIFIED), and the rest of the filter is the
// query's own.
//
// SCHEMATA_LINKS with no region qualifier is the US region's
// (unqualifiedSchemataLinks, #1392).
//
// SCHEMATA_LINKS is always empty: its rows are datasets linked through
// BigQuery sharing (Analytics Hub), which an instance does not serve; a
// Logging bucket's link (#1292) makes no BigQuery dataset.

const objectPrivilegesColumns = "STRUCT<object_catalog STRING, object_schema STRING, object_name STRING, " +
	"object_type STRING, privilege_type STRING, grantee STRING>"

const schemataLinksColumns = "STRUCT<catalog_name STRING, schema_name STRING, linked_schema_catalog_number STRING, " +
	"linked_schema_catalog_name STRING, linked_schema_name STRING, linked_schema_creation_time TIMESTAMP, " +
	"linked_schema_org_display_name STRING, shared_asset_id STRING, link_type STRING>"

func init() {
	infoSchemaBuilders["OBJECT_PRIVILEGES"] = front.objectPrivilegesRows
	infoSchemaBuilders["SCHEMATA_LINKS"] = emptyRows(schemataLinksColumns)
	regionViews["OBJECT_PRIVILEGES"], regionColumns["OBJECT_PRIVILEGES"] = true, objectPrivilegesColumns
	regionViews["SCHEMATA_LINKS"], regionColumns["SCHEMATA_LINKS"] = true, schemataLinksColumns
	// regionOnlyInfoSchema refuses a dataset-qualified reference (#1458).
}

// unqualifiedSchemataLinks returns sql's references to
// [project.]INFORMATION_SCHEMA.SCHEMATA_LINKS with no region qualifier
// (#1392), which BigQuery reads in the US region: each is answered as
// `region-us`.INFORMATION_SCHEMA.SCHEMATA_LINKS (withInformationSchemaJobs).
// A three-part name is the project's when its first part is project, the
// request's: another is a dataset's, refused 400 (regionOnlyInfoSchema),
// or another project's, 501.
func unqualifiedSchemataLinks(sql, project string) []jobsRef {
	t, ok := lex(sql)
	if !ok {
		return nil
	}
	var refs []jobsRef
	for i := 0; i < len(t); i++ {
		if t[i].kind != tokWord && t[i].kind != tokQuoted || i > 0 && t[i-1].punct(".") {
			continue
		}
		parts, next := path(t, i)
		n := len(parts)
		if n < 2 || n > 3 || !strings.EqualFold(parts[n-2], "INFORMATION_SCHEMA") || !strings.EqualFold(parts[n-1], "SCHEMATA_LINKS") ||
			n == 3 && parts[0] != project {
			i = next - 1
			continue
		}
		ref := jobsRef{pos: t[i].pos, end: t[next-1].end, region: "region-us", view: "SCHEMATA_LINKS"}
		if next < len(t) && (t[next].is("AS") || t[next].kind == tokQuoted ||
			t[next].kind == tokWord && !reservedWords[strings.ToUpper(t[next].text)]) {
			ref.alias = true
		}
		refs = append(refs, ref)
		i = next - 1
	}
	return refs
}

// legacyDatasetRoles are the dataset access roles BigQuery writes as IAM
// roles.
var legacyDatasetRoles = map[string]string{"READER": "roles/bigquery.dataViewer",
	"WRITER": "roles/bigquery.dataEditor", "OWNER": "roles/bigquery.dataOwner"}

// specialGroupGrantees are the special groups as OBJECT_PRIVILEGES names
// them.
var specialGroupGrantees = map[string]string{"projectOwners": "projectOwner", "projectWriters": "projectEditor",
	"projectReaders": "projectViewer"}

// accessGrantee is an access entry's grantee, or "" for one that grants
// no role.
func accessGrantee(e map[string]any) string {
	switch {
	case str(e, "userByEmail") != "":
		return "user:" + str(e, "userByEmail")
	case str(e, "groupByEmail") != "":
		return "group:" + str(e, "groupByEmail")
	case str(e, "domain") != "":
		return "domain:" + str(e, "domain")
	case str(e, "iamMember") != "":
		return str(e, "iamMember")
	case str(e, "specialGroup") != "":
		if g, ok := specialGroupGrantees[str(e, "specialGroup")]; ok {
			return g
		}
		return str(e, "specialGroup")
	}
	return ""
}

// objectPrivilegesRows returns the subquery of the bindings of dataset and
// its tables (above).
func (f front) objectPrivilegesRows(r *http.Request, dataset string, meta *datasetMeta) (string, int, string) {
	project := projectOf(f.base)
	st, got := f.get(r, "/datasets/"+url.PathEscape(dataset))
	if st == http.StatusNotFound {
		return "", http.StatusNotFound, fmt.Sprintf("Not found: Dataset %s:%s", project, dataset)
	}
	var ds struct {
		Access []map[string]any `json:"access"`
	}
	if st != http.StatusOK || json.Unmarshal(got, &ds) != nil {
		return "", http.StatusInternalServerError, "cloudburrow: could not read the dataset: " + errorMessage(got, st)
	}
	var rows [][]string
	for _, e := range ds.Access {
		grantee, role := accessGrantee(e), str(e, "role")
		if grantee == "" || role == "" {
			continue
		}
		if legacy, ok := legacyDatasetRoles[strings.ToUpper(role)]; ok {
			role = legacy
		}
		rows = append(rows, []string{sqlString(project), "NULL", sqlString(dataset), sqlString("SCHEMA"),
			sqlString(role), sqlString(grantee)})
	}
	if f.iam != nil {
		tables, st, msg := f.tablesOf(r, dataset, meta)
		if st != 0 {
			return "", st, msg
		}
		for _, t := range tables {
			id := str(t, "tableId")
			for _, b := range f.iam.bindings(iamKey(project, dataset, "tables", id), str(t, "creationTime")) {
				members := append([]string(nil), b.Members...)
				sort.Strings(members)
				for _, m := range members {
					rows = append(rows, []string{sqlString(project), sqlString(dataset), sqlString(id),
						sqlString(str(t, "type")), sqlString(b.Role), sqlString(m)})
				}
			}
		}
	}
	return rowsSubquery(objectPrivilegesColumns, rows), 0, ""
}

// namesObjectName reports whether sql names object_name after a WHERE,
// as a query of OBJECT_PRIVILEGES must filter on it.
func namesObjectName(sql string) bool {
	toks, _ := lex(sql)
	where := false
	for _, t := range toks {
		if t.is("WHERE") {
			where = true
		}
		if where && (t.kind == tokWord || t.kind == tokQuoted) && strings.EqualFold(t.text, "object_name") {
			return true
		}
	}
	return false
}
