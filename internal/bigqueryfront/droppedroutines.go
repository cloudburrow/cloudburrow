package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"net/url"
)

// A deleted dataset's routines and models, kept for its time travel window
// (#1483, dropped.go).
//
// BigQuery's undelete restores a dataset "to the state it was in when it
// was deleted" (https://cloud.google.com/bigquery/docs/restore-deleted-datasets,
// read 2026-10-16), and datasets.delete's deleteContents deletes "all the
// tables, views, routines and models in the dataset"
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/datasets/delete).
// Measured first (official Go client, through the front): after
// datasets.delete with deleteContents a function of the dataset still ran
// (the emulator leaves them in its engine, dropschema.go), and
// datasets.undelete failed 404, as the front's datasets.delete of the
// models (#1465) took the request before the deletion was kept; and the
// front kept no routine or model with a deletion.
//
// So a kept dataset deletion keeps, with the dataset, what the front holds
// of each routine and model: the Routine resource of each function and
// table function the engine still has (routinedefs.go), each stored
// procedure (procedures.go), and each model (models.go). When the
// deletion is carried out the functions are dropped in the engine and the
// front forgets the routines and models of the dataset (as DROP SCHEMA
// does). An undelete makes each function again through the emulator's
// routines.insert (as routines.update does), with the resource it had,
// creation and modification times among them, and puts the procedures and
// models back as they were.
//
// A function routines.insert made before the front started (#1522), whose
// definition the front never saw, is read from the emulator, which keeps
// each Routine routines.insert makes (bigquery-emulator patch 0053):
// routines.list of the dataset, for each routine the front does not know
// and the engine still has.

// keepRoutines records in d the dataset's routines and models.
func (f front) keepRoutines(r *http.Request, d *droppedDataset) {
	project := projectOf(f.base)
	for _, def := range f.liveRoutines(r, project, d.Dataset) {
		if def["routineType"] == "PROCEDURE" {
			continue
		}
		if b, err := json.Marshal(def); err == nil {
			d.Routines = append(d.Routines, b)
		}
	}
	d.Routines = append(d.Routines, f.emulatorRoutines(r, d.Dataset, d.Routines)...)
	if procs := f.functions.procsIn(project, d.Dataset); len(procs) > 0 {
		d.Procedures = procs
	}
	d.Models = f.models.list(project, d.Dataset)
}

// dropRoutines carries out the deletion of the routines and models kept
// in d, once its dataset is deleted.
func (f front) dropRoutines(r *http.Request, d *droppedDataset) {
	project := projectOf(f.base)
	for _, raw := range d.Routines {
		var def map[string]any
		if json.Unmarshal(raw, &def) != nil {
			continue
		}
		ref, _ := def["routineReference"].(map[string]any)
		id, _ := ref["routineId"].(string)
		if id == "" {
			continue
		}
		f.sendDDL(r, dropFunction(def["routineType"] == "TABLE_VALUED_FUNCTION")+" IF EXISTS "+functionName([]string{d.Dataset, id}))
	}
	f.functions.forget(project, d.Dataset)
	f.models.dropDataset(project, d.Dataset)
}

// restoreRoutines makes again the routines and models kept in d, in its
// dataset just made again, and returns why it could not, or "".
func (f front) restoreRoutines(r *http.Request, d droppedDataset) string {
	project := projectOf(f.base)
	for _, raw := range d.Routines {
		var def map[string]any
		if json.Unmarshal(raw, &def) != nil {
			continue
		}
		ref, _ := def["routineReference"].(map[string]any)
		id, _ := ref["routineId"].(string)
		if id == "" {
			continue
		}
		insert := map[string]any{}
		for k, v := range def {
			insert[k] = v
		}
		for _, k := range []string{"etag", "creationTime", "lastModifiedTime"} {
			delete(insert, k)
		}
		body, _ := json.Marshal(insert)
		if st, got := f.send(r, http.MethodPost, "/datasets/"+url.PathEscape(d.Dataset)+"/routines", body); st != http.StatusOK {
			return "routine " + d.Dataset + "." + id + ": " + errorMessage(got, st)
		}
		if f.functions != nil {
			f.functions.add(project, []string{d.Dataset, id})
			f.functions.setDef(project, []string{d.Dataset, id}, def)
		}
	}
	for name, p := range d.Procedures {
		f.functions.setProc(project, []string{d.Dataset, name}, p)
	}
	if f.models != nil {
		for _, m := range d.Models {
			f.models.put(m)
		}
	}
	return ""
}

// emulatorRoutines are the Routine resources the emulator keeps of the
// functions of dataset (bigquery-emulator patch 0053) that are not among
// known, the front's, and that the engine still has (above, #1522).
func (f front) emulatorRoutines(r *http.Request, dataset string, known []json.RawMessage) []json.RawMessage {
	have := map[string]bool{}
	for _, raw := range known {
		have[routineIDOf(raw)] = true
	}
	st, got := f.get(r, "/datasets/"+url.PathEscape(dataset)+"/routines")
	var list struct {
		Routines []json.RawMessage `json:"routines"`
	}
	if st != http.StatusOK || json.Unmarshal(got, &list) != nil {
		return nil
	}
	var out []json.RawMessage
	for _, raw := range list.Routines {
		var def map[string]any
		if json.Unmarshal(raw, &def) != nil {
			continue
		}
		id := routineIDOf(raw)
		typ, _ := def["routineType"].(string)
		if id == "" || have[id] || typ == "PROCEDURE" {
			continue
		}
		if p, ok := f.functions.proc(projectOf(f.base), []string{dataset, id}); ok && p.Routine != nil {
			continue // the front's own procedure (procedures.go)
		}
		if !f.routineExists(r, dataset, id) {
			continue // dropped since, by DDL or routines.delete
		}
		have[id] = true
		out = append(out, raw)
	}
	return out
}

// routineIDOf is the routineId of a Routine resource.
func routineIDOf(raw json.RawMessage) string {
	var def struct {
		Ref struct {
			ID string `json:"routineId"`
		} `json:"routineReference"`
	}
	_ = json.Unmarshal(raw, &def)
	return def.Ref.ID
}
