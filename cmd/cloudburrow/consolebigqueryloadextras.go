package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"slices"
	"strings"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The load options the official Go client's loader has no field for
// (#1547): hivePartitioningOptions (mode and sourceUriPrefix) and
// jsonExtension GEOJSON, which the front serves for a
// NEWLINE_DELIMITED_JSON load (hive partitioning for Parquet too) (internal/bigqueryfront/loadvariants.go).
// The form's load is still sent by the official client, through a
// transport that adds the two to the job's configuration.load in
// jobs.insert's body (JSON, or the metadata part of a multipart upload).
// Hive partitioning reads its keys from Cloud Storage paths, so Load from a
// file refuses it.

// bigqueryHiveModes are hivePartitioningOptions.mode's values, and NONE.
var bigqueryHiveModes = []string{"NONE", "AUTO", "STRINGS", "CUSTOM"}

// bigqueryJSONExtensions are jsonExtension's values, and NONE.
var bigqueryJSONExtensions = []string{"NONE", "GEOJSON"}

// bigqueryLoadExtraFields are the form's inputs for them.
func bigqueryLoadExtraFields() []console.Field {
	return []console.Field{
		{Name: "jsonExtension", Label: "JSON extension", Type: "select", Options: bigqueryJSONExtensions,
			Section: "JSON options",
			Help: "Newline-delimited JSON only: GEOJSON reads each line as a GeoJSON Feature, its geometry a " +
				"GEOGRAPHY column named geometry and each member of its properties a column."},
		{Name: "hiveMode", Label: "Source data partitioning", Type: "select", Options: bigqueryHiveModes,
			Section: "Hive partitioning",
			Help: "Cloud Storage, newline-delimited JSON or Parquet: read partition keys from directories key=value " +
				"after the prefix below, as columns: AUTO detects STRING, INTEGER, DATE or TIMESTAMP, STRINGS makes " +
				"each a STRING, CUSTOM takes the types the prefix encodes, as gs://bucket/table/{dt:DATE}/{n:INTEGER}."},
		{Name: "hiveSourceUriPrefix", Label: "Source URI prefix", Type: "text", Section: "Hive partitioning",
			Help: "The gs:// URI every file is under, ending where the key=value directories begin."},
	}
}

// loadExtras are those options, from the form.
type loadExtras struct {
	hive          map[string]any
	jsonExtension string
}

// loadExtrasOf reads them; fromStorage is whether the load reads Cloud
// Storage (Load from Cloud Storage) rather than a file. It is nil when
// the form sets neither.
func loadExtrasOf(values map[string]string, fromStorage bool) (*loadExtras, error) {
	format := values["format"]
	if format == "" {
		format = "CSV"
	}
	var x loadExtras
	switch ext := strings.TrimSpace(values["jsonExtension"]); ext {
	case "", "NONE":
	case "GEOJSON":
		if format != "NEWLINE_DELIMITED_JSON" {
			return nil, fmt.Errorf("jsonExtension GEOJSON applies to NEWLINE_DELIMITED_JSON only, and this load is %s", format)
		}
		x.jsonExtension = ext
	default:
		return nil, fmt.Errorf("the JSON extension is NONE or GEOJSON, not %q", ext)
	}
	mode := strings.TrimSpace(values["hiveMode"])
	prefix := strings.TrimSpace(values["hiveSourceUriPrefix"])
	switch {
	case mode == "" || mode == "NONE":
		if prefix != "" {
			return nil, errors.New("a source URI prefix needs a source data partitioning mode (AUTO, STRINGS or CUSTOM)")
		}
	case !slices.Contains(bigqueryHiveModes, mode):
		return nil, fmt.Errorf("the source data partitioning is one of %s, not %q", strings.Join(bigqueryHiveModes, ", "), mode)
	case !fromStorage:
		return nil, errors.New("hive partitioning reads its keys from Cloud Storage paths: use Load from Cloud Storage")
	case !strings.HasPrefix(prefix, "gs://"):
		return nil, fmt.Errorf("hive partitioning needs a gs:// source URI prefix, not %q", prefix)
	default:
		x.hive = map[string]any{"mode": mode, "sourceUriPrefix": prefix}
	}
	if x.hive == nil && x.jsonExtension == "" {
		return nil, nil
	}
	return &x, nil
}

// loadClient is p's BigQuery client, sending x in each load's jobs.insert
// when x is not nil.
func (p bigqueryProvider) loadClient(ctx context.Context, x *loadExtras) (*bigquery.Client, error) {
	if x == nil {
		return p.client(ctx)
	}
	c, err := bigquery.NewClient(ctx, p.project, option.WithEndpoint("http://"+p.endpoint),
		option.WithHTTPClient(&http.Client{Transport: loadExtrasTransport{x: x, next: http.DefaultTransport}}))
	if err != nil {
		return nil, fmt.Errorf("cannot reach BigQuery: %w", err)
	}
	return c, nil
}

// loadExtrasTransport adds x to the configuration.load of each jobs.insert
// it sends.
type loadExtrasTransport struct {
	x    *loadExtras
	next http.RoundTripper
}

func (t loadExtrasTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/jobs") || req.Body == nil {
		return t.next.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		return nil, err
	}
	if out, err := t.x.edit(req.Header.Get("Content-Type"), body); err == nil {
		body = out
	}
	r2 := req.Clone(req.Context())
	r2.Body = io.NopCloser(bytes.NewReader(body))
	r2.ContentLength = int64(len(body))
	r2.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return t.next.RoundTrip(r2)
}

// edit returns body, a jobs.insert of content type ct, with x in its
// configuration.load.
func (x *loadExtras) edit(ct string, body []byte) ([]byte, error) {
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(mt, "multipart/") {
		return x.editJob(body)
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var out bytes.Buffer
	mw := multipart.NewWriter(&out)
	if err := mw.SetBoundary(params["boundary"]); err != nil {
		return nil, err
	}
	for i := 0; ; i++ {
		part, err := mr.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			if data, err = x.editJob(data); err != nil {
				return nil, err
			}
		}
		w, err := mw.CreatePart(textproto.MIMEHeader(part.Header))
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// editJob returns job, a Job's JSON, with x in its configuration.load.
func (x *loadExtras) editJob(job []byte) ([]byte, error) {
	var j map[string]any
	dec := json.NewDecoder(bytes.NewReader(job))
	dec.UseNumber()
	if err := dec.Decode(&j); err != nil {
		return nil, err
	}
	conf, _ := j["configuration"].(map[string]any)
	load, _ := conf["load"].(map[string]any)
	if load == nil {
		return nil, errors.New("not a load")
	}
	if x.hive != nil {
		load["hivePartitioningOptions"] = x.hive
	}
	if x.jsonExtension != "" {
		load["jsonExtension"] = x.jsonExtension
	}
	return json.Marshal(j)
}
