package bigqueryfront

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// A load whose fileSetSpecType is FILE_SET_SPEC_TYPE_NEW_LINE_DELIMITED_MANIFEST
// (#1547).
//
// JobConfigurationLoad documents it
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfigurationLoad,
// read 2026-11-17): fileSetSpecType "Specifies how source URIs are
// interpreted for constructing the file set to load. By default, source
// URIs are expanded against the underlying storage. You can also specify
// manifest files to control how the file set is constructed";
// FILE_SET_SPEC_TYPE_FILE_SYSTEM_MATCH "expands source URIs by listing
// files from the object store. It is the default behavior if
// FileSetSpecType is not set"; FILE_SET_SPEC_TYPE_NEW_LINE_DELIMITED_MANIFEST
// "indicates that the provided URIs are newline-delimited manifest files,
// with one URI per line. Wildcard URIs are not supported." "Query open
// table formats with manifests"
// (https://cloud.google.com/bigquery/docs/query-open-table-format-using-manifest-files)
// names the same format: "a newline-delimited list of URIs".
//
// So the front reads each manifest the load names from the instance's
// Cloud Storage, and sends the load on with the URIs they list, in order,
// as its sourceUris and without fileSetSpecType: the path every load from
// Cloud Storage takes. The job answers with the client's
// configuration.load (clientLoad, #998). A source URI or a listed URI with
// a "*" is 400 invalid ("Wildcard URIs are not supported"), as is a listed
// line that is not a gs:// URI and another fileSetSpecType. UNVERIFIED
// (docs re-read 2026-11-17, both pages above, still unanswered): an
// empty line (and a line's surrounding spaces and "\r") is not a URI, and a
// manifest listing none is 400 invalid; a listed object that does not
// exist is 404 notFound, as a source URI that names none.

// manifestSpec is the fileSetSpecType that names manifests.
const manifestSpec = "FILE_SET_SPEC_TYPE_NEW_LINE_DELIMITED_MANIFEST"

// maxManifestBytes bounds a manifest the front reads into memory.
const maxManifestBytes = 64 << 20

// loadManifests expands the manifests of the load in r (see above). It
// returns the client's configuration.load when it rewrote the job (or
// nil), and ok false when it has answered w.
func (f front) loadManifests(w http.ResponseWriter, r *http.Request) (json.RawMessage, bool) {
	var job struct {
		Configuration struct {
			Load *struct {
				SourceURIs      []string `json:"sourceUris"`
				FileSetSpecType string   `json:"fileSetSpecType"`
			} `json:"load"`
		} `json:"configuration"`
	}
	if !decodeJob(r, &job) || job.Configuration.Load == nil {
		return nil, true
	}
	l := job.Configuration.Load
	fail := func(code int, reason, msg string) (json.RawMessage, bool) {
		writeError(w, code, reason, msg)
		return nil, false
	}
	switch strings.ToUpper(l.FileSetSpecType) {
	case "", "FILE_SET_SPEC_TYPE_FILE_SYSTEM_MATCH":
		return nil, true
	case manifestSpec:
	default:
		return fail(http.StatusBadRequest, "invalid", fmt.Sprintf("Invalid fileSetSpecType %q: it is "+
			"FILE_SET_SPEC_TYPE_FILE_SYSTEM_MATCH or %s.", l.FileSetSpecType, manifestSpec))
	}
	if len(l.SourceURIs) == 0 {
		return fail(http.StatusBadRequest, "invalid", "A load with fileSetSpecType "+manifestSpec+" reads its files' "+
			"URIs from the manifests its sourceUris name: give it sourceUris, not an upload.")
	}
	for _, u := range l.SourceURIs {
		if strings.Contains(u, "*") {
			return fail(http.StatusBadRequest, "invalid", fmt.Sprintf("Invalid source URI %q: with fileSetSpecType %s "+
				"the URIs are manifest files, and \"Wildcard URIs are not supported\".", u, manifestSpec))
		}
	}
	if f.storage == nil {
		writeNoStorage(w, l.SourceURIs)
		return nil, false
	}
	kept := clientLoad(r, l.SourceURIs)
	objs, err := f.storage.resolve(r.Context(), l.SourceURIs)
	if err != nil {
		e := asLoadDataError(err)
		return fail(e.code, e.reason, e.msg)
	}
	var listed []any
	for _, o := range objs {
		rc, err := f.storage.open(r.Context(), o)
		if err != nil {
			e := asLoadDataError(err)
			return fail(e.code, e.reason, e.msg)
		}
		uris, msg := manifestURIs(io.LimitReader(rc, maxManifestBytes+1), o.uri())
		rc.Close()
		if msg != "" {
			return fail(http.StatusBadRequest, "invalid", msg+" Nothing was loaded.")
		}
		for _, u := range uris {
			listed = append(listed, u)
		}
	}
	if !editJob(r, func(j map[string]any) bool {
		conf, _ := j["configuration"].(map[string]any)
		load, _ := conf["load"].(map[string]any)
		if load == nil {
			return false
		}
		load["sourceUris"] = listed
		delete(load, "fileSetSpecType")
		return true
	}) {
		return fail(http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the load's sourceUris")
	}
	return kept, true
}

// manifestURIs reads a manifest's URIs, one per line, or why it is
// refused. loc is the manifest's gs:// URI.
func manifestURIs(data io.Reader, loc string) ([]string, string) {
	sc := bufio.NewScanner(data)
	sc.Buffer(make([]byte, 0, 64<<10), maxManifestBytes)
	var out []string
	var size int
	for line := 1; sc.Scan(); line++ {
		size += len(sc.Bytes()) + 1
		if size > maxManifestBytes {
			return nil, fmt.Sprintf("The manifest %s is larger than CloudBurrow reads (%d bytes).", loc, maxManifestBytes)
		}
		u := strings.TrimSpace(sc.Text())
		if u == "" {
			continue
		}
		switch {
		case strings.Contains(u, "*"):
			return nil, fmt.Sprintf("The manifest %s lists %q (line %d): \"Wildcard URIs are not supported.\"", loc, u, line)
		case !strings.HasPrefix(u, "gs://"):
			return nil, fmt.Sprintf("The manifest %s lists %q (line %d), which is not a gs:// URI.", loc, u, line)
		}
		out = append(out, u)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Sprintf("The manifest %s could not be read: %v.", loc, err)
	}
	if len(out) == 0 {
		return nil, fmt.Sprintf("The manifest %s lists no URI.", loc)
	}
	return out, ""
}
