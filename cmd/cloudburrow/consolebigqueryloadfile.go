package main

// Load from a file (#999): a BigQuery load of a file chosen in the browser,
// on a dataset's and a table's page, beside Load from Cloud Storage (#993).
//
// The form is Load from Cloud Storage's with a file control where the URIs
// were: the same formats, write preferences, schema, auto-detect and CSV
// options (loadFileConfig), since the front puts a load of the client's own
// data through the same checks as one from Cloud Storage (#919, #931). The
// file reaches the console on its upload route (POST
// /api/actions/bigquery/upload), under the upload limit in Settings, and is
// streamed to the front through the official Go client's ReaderSource as it
// arrives: a file of at most bigqueryUploadChunk bytes goes as one multipart
// upload (uploadType=multipart), a larger one as a resumable upload in
// chunks of that size (uploadType=resumable), so the console holds one chunk
// of it at a time. The job it ran answers, linked to Job history, as Load
// from Cloud Storage's does.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const actLoadFile = "loadfile"

// bigqueryUploadChunk is the Go client's upload chunk: a file larger than it
// is sent as a resumable upload, one chunk at a time.
const bigqueryUploadChunk = 4 << 20

// bigqueryFileLoadTimeout bounds one Load from a file: the upload and the
// job, inside the upload route's thirty minutes. A load from Cloud Storage
// reads what is already in the instance; this one waits on the browser.
const bigqueryFileLoadTimeout = 10 * time.Minute

// bigqueryLoadFileFields are Load from a file's inputs: Load from Cloud
// Storage's, with the file where the URIs were.
func bigqueryLoadFileFields(withTable bool) []console.Field {
	fields := bigqueryLoadFields(withTable)
	for i, f := range fields {
		if f.Name == "uris" {
			fields[i] = console.Field{Name: "file", Label: "File", Type: console.FileFieldType, Required: true,
				Help: "A CSV, newline-delimited JSON or Parquet file on this computer, of the format chosen below, " +
					"at most the upload limit set in Settings and utilities."}
		}
	}
	return fields
}

// ActAtFile implements console.FileActor: Load from a file.
func (p bigqueryProvider) ActAtFile(ctx context.Context, project string, path []string, action string, values map[string]string, file console.UploadedFile) (*console.Listing, error) {
	if err := p.writable(project); err != nil {
		return nil, err
	}
	if action != actLoadFile || len(path) < 1 || len(path) > 2 {
		return nil, fmt.Errorf("unknown action %q with a file", action)
	}
	tableID := strings.TrimSpace(values["tableId"])
	if len(path) == 2 {
		tableID = path[1]
	}
	return p.loadFile(ctx, project, path[0], tableID, values, file)
}

// loadFile runs Load from a file into datasetID.tableID and waits for it.
func (p bigqueryProvider) loadFile(ctx context.Context, project, datasetID, tableID string, values map[string]string, file console.UploadedFile) (*console.Listing, error) {
	if tableID == "" {
		return nil, errors.New("a table ID is required")
	}
	cfg, err := loadFileConfig(values)
	if err != nil {
		return nil, err
	}
	write := values["writeDisposition"]
	if write == "" {
		write = "WRITE_APPEND"
	}
	if !slices.Contains(bigqueryWriteModes, write) {
		return nil, fmt.Errorf("the write preference is WRITE_APPEND or WRITE_TRUNCATE, not %q", write)
	}
	ctx, cancel := context.WithTimeout(ctx, bigqueryFileLoadTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	src := bigquery.NewReaderSource(file)
	src.FileConfig = *cfg
	loader := c.Dataset(datasetID).Table(tableID).LoaderFrom(src)
	loader.WriteDisposition = bigquery.TableWriteDisposition(write)
	loader.MediaOptions = []googleapi.MediaOption{googleapi.ChunkSize(bigqueryUploadChunk)}
	job, err := loader.Run(ctx)
	if err != nil {
		return nil, bigqueryRefusal(err)
	}
	return p.finishedJob(ctx, project, job)
}

var _ console.FileActor = bigqueryProvider{}
