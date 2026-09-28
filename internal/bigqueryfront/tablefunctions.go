package bigqueryfront

import (
	"net/http"
	"strings"
)

// Table functions crash the emulator (#1043).
//
// The pinned emulator's SQL engine (goccy/googlesqlite v0.3.1, over
// goccy/go-googlesql v0.3.0, a WebAssembly GoogleSQL translated to Go)
// frees every table function it registers, while its catalog still points
// at it. Read in the pinned source, not changed: googlesqlite's
// internal/catalog.go, tvfHandleForSpec, builds each table function with
// go-googlesql's NewFixedOutputSchemaTVF and returns only the embedded
// *TableValuedFunction (`return tvf.TableValuedFunction, nil`), which
// addTVFSpecRecursiveImpl adds to the root catalog and the dataset's
// sub-catalog (AddTableValuedFunction2, which does not take ownership).
// In go-googlesql the finalizer that deletes the C++ object is on the
// outer *FixedOutputSchemaTVF (newFixedOutputSchemaTVF); the embedded
// handle has none, and the catalog's keep-alive pins only the embedded
// handle. So once the Go garbage collector next runs, the outer handle's
// finalizer deletes the table function, and every later analysis that
// looks it up reads freed WebAssembly memory. The catalog registers the
// function again, with a new handle freed the same way, whenever it is
// rebuilt (a DROP TABLE, for one).
//
// Measured against the pinned image on the emulator's own REST port (no
// front), on a fresh emulator: `CREATE TABLE FUNCTION ds.tf(x INT64) AS
// (SELECT x AS y)`, then 60 queries that allocate (so that a collection
// runs), then `SELECT * FROM ds.tf(1)`: the emulator exited with SIGSEGV in
// googlesqlwasm2go's p6.Fn16235, called from AnalyzeStatementFromParserAST,
// the crash of #1043, and Kubernetes restarted it with every dataset lost.
// The same sequence with a scalar `CREATE FUNCTION ds.f(x INT64) AS (x +
// 1)` read `ds.f(1)` = 2 each time. Through the front, the BigQuery compat
// suite's TestBigQueryCreateFunctionOfAnExistingFunction, which made a
// table function, failed "Table-valued function not found" (freed memory
// reused) on an instance that had run the suite once, and the emulator
// then crashed the same way. The front is not needed to set it off (the
// emulator alone crashed above); in #1043 its probes and its reads of
// tabledata.list by query ran more queries, and so more collections,
// between a table function's creation and its next use.
//
// Nothing the front can send keeps a table function alive, so it makes
// none: CREATE [OR REPLACE] [TEMP] TABLE FUNCTION, in a query or a script,
// and routines.insert of a TABLE_VALUED_FUNCTION routine are 501, before
// anything runs (DROP TABLE FUNCTION was 501 already: the engine does not
// support it, checkDDL). The 501 goes once an
// emulator release keeps the outer handle (or its finalizer is moved to
// the embedded one).

// tableFunctionMsg is the 501 message for a statement or routine that
// makes a table function.
const tableFunctionMsg = "Not implemented here: making a table function (CREATE TABLE FUNCTION, or routines.insert " +
	"of a TABLE_VALUED_FUNCTION routine). BigQuery makes it, but the SQL engine of the emulator behind CloudBurrow " +
	"frees a table function while its catalog still points at it, so a later query of it fails (\"Table-valued " +
	"function not found\") or crashes the emulator, which loses every dataset (#1043). Nothing was run. Use a " +
	"view, or a query with the function's body."

// makesTableFunction reports whether v's statements make a table
// function.
func makesTableFunction(v ddlVerdict) bool {
	for _, s := range v.funcs {
		if s.table && !s.drop {
			return true
		}
	}
	return false
}

// refuseTableFunctionRoutine answers routines.insert, r, of a
// TABLE_VALUED_FUNCTION routine with 501, and reports whether it did.
func refuseTableFunctionRoutine(w http.ResponseWriter, r *http.Request) bool {
	var body struct {
		RoutineType string `json:"routineType"`
	}
	if _, ok := decode(r, &body); !ok || !strings.EqualFold(body.RoutineType, "TABLE_VALUED_FUNCTION") {
		return false
	}
	writeError(w, http.StatusNotImplemented, "notImplemented", tableFunctionMsg)
	return true
}
