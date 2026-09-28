package bigqueryfront

import "bytes"

// Scratch tables in resultsDataset (#1057).
//
// The front makes a table of its own, a scratch table (scratchTable), for
// a few statements the emulator cannot carry out as BigQuery does, and
// deletes it before it answers: a copy job with WRITE_TRUNCATE
// (copyjob.go), a load with autodetect of a FLOAT column (floattype.go), a
// MERGE whose source is a subquery (dml.go), CREATE OR REPLACE TABLE
// (script.go, replace), a Parquet load the front carries out
// (parquetload.go) and a schema update that adds columns (schemaupdate.go). Deleting one is a DROP TABLE, which the pinned
// emulator's SQL engine answers by building every catalog again, with
// every builtin function in each, for every dataset that holds a table
// (googlesqlite v0.3.1, internal/catalog.go, resetCatalog): the cost of
// one grows with the datasets, and the memory it takes is given back only
// when the emulator's Go heap is collected (engine.go).
//
// So a scratch table is made in resultsDataset, the hidden dataset query
// results are written to (results.go), which the front makes first when
// it has to, and which holds tables already, so it adds no catalog; and
// the front's tables.delete of it is answered 204 at once and carried out
// later, with the expired query results, in batches while the instance is
// idle (resultsexpiry.go). A client sees the same answers; the table is
// listed in resultsDataset (tables.list of it) until then. A scratch table
// a failed statement leaves holding a client's rows (floattype.go) is not
// deleted, and its error names it.

// scratchPrefix begins the name of every scratch table (scratchTable).
const scratchPrefix = "_cloudburrow_"

// unscratch returns an error the emulator gave about scratch table
// scratch with its dataset, resultsDataset, written as dataset, or left
// out when dataset is "", so that the answer names no hidden dataset.
func unscratch(got []byte, scratch, dataset string) []byte {
	with := ""
	if dataset != "" {
		with = dataset + "."
	}
	for _, p := range []string{resultsDataset + "." + scratch, resultsDataset + ":" + scratch} {
		got = bytes.ReplaceAll(got, []byte(p), []byte(with+scratch))
	}
	return got
}
