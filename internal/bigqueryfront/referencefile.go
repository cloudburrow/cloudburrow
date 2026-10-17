package bigqueryfront

import (
	"net/http"
)

// referenceFileSchemaUri of an Avro or Parquet load (#1457).
//
// BigQuery's JobConfigurationLoad: "The user can provide a reference file
// with the reader schema. This file is only loaded if it is part of source
// URIs, but is not loaded otherwise. It is enabled for the following
// formats: AVRO, PARQUET, ORC"; the Avro and Parquet loading pages: "To
// provide a table schema ... set the referenceFileSchemaUri property ...
// to the URL of the reference file", where without one "the alphabetically
// last file is used". So the table's schema is the reference file's,
// whether or not its own rows are loaded, and a reference file that does
// not exist is 404 notFound, as any source URI is (storageReader.resolve).
//
// The front loads the files whose schema is the reference file's, by
// BigQuery's conversions of that schema. A data file whose schema differs
// from the reference file's is 501: BigQuery reads it by the reference
// file as its reader schema, and its documentation does not say how a
// file's columns are resolved to another schema's. So are a reference URI
// with a wildcard (the reference is "a reference file") and a load with no
// Cloud Storage for the front to read the reference file from.

// referenceObject resolves a load's referenceFileSchemaUri to its object,
// or returns the status and message of why it cannot.
func (f front) referenceObject(r *http.Request, uri string) (gsObject, int, string) {
	if f.storage == nil {
		return gsObject{}, http.StatusNotImplemented, "with referenceFileSchemaUri and no Cloud Storage for CloudBurrow " +
			"to read the reference file from"
	}
	if severalObjects([]string{uri}) {
		return gsObject{}, http.StatusNotImplemented, "whose referenceFileSchemaUri " + uri + " has a wildcard: BigQuery " +
			"documents a reference file, not several, and does not say which of several it would read"
	}
	objs, err := f.storage.resolve(r.Context(), []string{uri})
	if err != nil {
		e := asLoadDataError(err)
		return gsObject{}, e.code, e.msg
	}
	return objs[0], 0, ""
}

const referenceDiffersWhy = "; BigQuery reads each file by the reference file's schema as its reader schema, and its " +
	"documentation does not say how a file's columns are resolved to another schema's. Load files whose schema is the " +
	"reference file's"
