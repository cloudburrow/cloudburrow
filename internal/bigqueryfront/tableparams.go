package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Table parameters of a table function (#2120).
//
// BigQuery: a table function's parameter may be a table, TABLE<column
// type, ...>, which "accepts an input table containing a superset of those
// columns in any order", or ANY TABLE; a call gives it as TABLE
// dataset.table or a subquery
// (https://cloud.google.com/bigquery/docs/table-functions#table_parameters).
// A routine's argument of one is argumentKind FIXED_TABLE, with its
// tableType ("Set if argument_kind == FIXED_TABLE"), or ANY_TABLE ("The
// argument is any table type"), and has no dataType ("Set if argument_kind
// == FIXED_TYPE",
// https://cloud.google.com/bigquery/docs/reference/rest/v2/routines#Argument).
//
// The engine CloudBurrow builds runs them (third_party/bigquery-emulator,
// googlesqlite patch 0109). The front leaves a table parameter's name in
// the function's body unqualified, and qualifies TABLE name in a call as
// it qualifies any table (qualify.go); routineFromDDL reads the
// parameters as the Routine's arguments. The emulator's routines.insert
// writes each argument as name and typeKind alone, so it cannot make a
// function of a table argument (it answered "missing data type"): the
// front makes one with the CREATE TABLE FUNCTION statement of the
// resource (insertTableRoutine).

// tableArguments reports whether a routine resource has a table argument.
func tableArguments(def map[string]any) bool {
	for _, a := range arguments(def) {
		switch strings.ToUpper(str(a, "argumentKind")) {
		case "FIXED_TABLE", "ANY_TABLE":
			return true
		}
	}
	return false
}

// insertTableRoutine answers routines.insert of a routine with a table
// argument (above), def the request's resource, in dataset. It reports
// whether it answered: a routine without one is the emulator's.
func (f front) insertTableRoutine(w http.ResponseWriter, r *http.Request, project, dataset string, def map[string]any) bool {
	if !tableArguments(def) {
		return false
	}
	ref, _ := def["routineReference"].(map[string]any)
	id := str(ref, "routineId")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid", "Missing required field: routine.routineReference.routineId.")
		return true
	}
	if str(def, "routineType") != "TABLE_VALUED_FUNCTION" {
		writeError(w, http.StatusBadRequest, "invalid", "Invalid value for routine "+project+":"+dataset+"."+id+
			": an argument of argumentKind FIXED_TABLE or ANY_TABLE is a table parameter, which only a TABLE_VALUED_FUNCTION takes.")
		return true
	}
	for _, a := range arguments(def) {
		if strings.EqualFold(str(a, "argumentKind"), "FIXED_TABLE") {
			tt, _ := a["tableType"].(map[string]any)
			if cols, _ := tt["columns"].([]any); len(cols) == 0 {
				writeError(w, http.StatusBadRequest, "invalid", "Invalid value for argument "+str(a, "name")+
					" of routine "+project+":"+dataset+"."+id+": an argument of argumentKind FIXED_TABLE needs its tableType's columns.")
				return true
			}
		}
	}
	if strings.TrimSpace(str(def, "definitionBody")) == "" {
		writeError(w, http.StatusBadRequest, "invalid", "Missing required field: routine.definitionBody.")
		return true
	}
	if f.routineExists(r, dataset, id) {
		writeError(w, http.StatusConflict, "duplicate", "Already Exists: Routine "+project+":"+dataset+"."+id)
		return true
	}
	var args []string
	for _, a := range arguments(def) {
		args = append(args, quotePath([]string{str(a, "name")})+" "+argumentType(a))
	}
	ddl := "CREATE TABLE FUNCTION " + quotePath([]string{project, dataset, id}) + "(" + strings.Join(args, ", ") + ")"
	if rt, ok := def["returnTableType"]; ok {
		ddl += " RETURNS " + tableTypeSQL(rt)
	}
	ddl += " AS (\n" + str(def, "definitionBody") + "\n)"
	legacy := false
	body, err := json.Marshal(queryOptions{Query: ddl, UseLegacySQL: &legacy})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internalError", err.Error())
		return true
	}
	if status, got := f.send(r, http.MethodPost, "/queries", body); status != http.StatusOK {
		writeRaw(w, status, got)
		return true
	}
	ref["projectId"] = project
	now := strconv.FormatInt(f.now().UnixMilli(), 10)
	def["creationTime"], def["lastModifiedTime"], def["etag"] = now, now, etagOf(now)
	if def["language"] == nil {
		def["language"] = "SQL"
	}
	f.functions.add(project, []string{dataset, id})
	f.functions.setDef(project, []string{dataset, id}, def)
	writeJSON(w, http.StatusOK, def)
	return true
}

// readRoutineRequest reads routines.insert's resource and puts the body
// back for the emulator; nil when it is not a JSON object.
func readRoutineRequest(r *http.Request) map[string]any {
	if r.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(r.Body)
	r.Body.Close()
	r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(raw)), int64(len(raw))
	if err != nil {
		return nil
	}
	var def map[string]any
	if json.Unmarshal(raw, &def) != nil {
		return nil
	}
	return def
}
