package bigqueryfront

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// The Storage Read API of a table whose ID another dataset has (#1046).
//
// The emulator reads a session's rows by the bare table ID (storageread.go,
// #1032), so a session of such a table would read another dataset's. But
// it reads the session's schema by the whole name, and it writes the rows
// as Arrow or Avro itself; and a bare ID no other table has names that
// table. So the front has the emulator read such a table through a view
// of its own, whose ID no client table has: a view in readAliasDataset,
// aliasID, whose query is SELECT * FROM `dataset.table` (the engine reads
// a name with its dataset as that dataset's table, qualify.go), and whose
// schema is the table's own, as the emulator's tables.get of the table
// gives it. A view made through tables.insert gets the schema the engine
// reads of its query (measured: every column NULLABLE, a REQUIRED one's
// mode and a description lost), so the front sets the table's with
// tables.patch after, which the emulator stores as sent (measured); the
// emulator then writes the view's rows with the schema it would have
// written the table's with, REQUIRED columns non-nullable in Arrow and
// without a union in Avro.
//
// CreateReadSession of such a table is sent to the emulator for the view,
// with the client's read options, format and stream count; its answer
// names the client's table, and an Avro schema the client's table's
// namespace and name (TableToAVRO, in the emulator's types/avro.go, writes
// them from the table the session reads). ReadRows of its streams reads
// the view. A session of a table whose ID was its own when it was made,
// and is another dataset's too when ReadRows asks for its rows, is made
// again for the view, with the client's request, and its rows are read
// from there.
//
// The view is kept, for the next session of the table: made when a
// session first needs it, and made again when the table's schema is no
// longer the view's (a column added, the table made again), since the
// view's query was read when it was made. Deleting a view is a DROP
// TABLE, which rebuilds every catalog of the engine (#1017, results.go),
// so views are not deleted after a read. The emulator keeps them until it
// restarts, when it keeps nothing; the next session makes the view again.
//
// Rows are read as the emulator reads a view's, when ReadRows asks for
// them, as it reads a table's (not a snapshot at the session's creation,
// as BigQuery's are).

// readAliasDataset is the dataset of the front's views of tables whose ID
// another dataset has. Its leading underscore makes it hidden, as
// resultsDataset is (results.go).
const readAliasDataset = "_cloudburrow_storage_read"

// aliasID is the ID of the front's view of dataset.table: one no client
// table has, as no client names a table so.
func aliasID(dataset, table string) string {
	sum := sha256.Sum256([]byte(dataset + "\x00" + table))
	return "_cloudburrow_read_" + hex.EncodeToString(sum[:16])
}

// path is t's resource name in the Storage Read API.
func (t readTable) path() string {
	return "projects/" + t.project + "/datasets/" + t.dataset + "/tables/" + t.table
}

// aliasOf returns the view the emulator reads t through (above), made or
// made again when needed; found is false when t does not exist, and the
// client's request is then sent as it is, for the emulator's own answer.
func (s *storageRead) aliasOf(ctx context.Context, t readTable) (alias readTable, found bool, err error) {
	s.aliasMu.Lock()
	defer s.aliasMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, storageReadTimeout)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://bigquery/", nil)
	if err != nil {
		return readTable{}, false, status.Error(codes.Internal, err.Error())
	}
	f := front{next: s.rest, base: "/bigquery/v2/projects/" + url.PathEscape(t.project)}
	failed := func(what string, code int, body []byte) error {
		return status.Errorf(codes.Unavailable, "CloudBurrow could not %s for a read session of %s.%s, "+
			"whose table ID another dataset has too (#1046): the emulator answered %d %s. Nothing was read.",
			what, t.dataset, t.table, code, bytes.TrimSpace(body))
	}

	code, body := f.get(r, tablePath(t.dataset, t.table))
	if code == http.StatusNotFound {
		return readTable{}, false, nil
	}
	var table struct {
		Schema json.RawMessage `json:"schema"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &table) != nil {
		return readTable{}, false, failed("read the table", code, body)
	}
	alias = readTable{project: t.project, dataset: readAliasDataset, table: aliasID(t.dataset, t.table)}
	query := "SELECT * FROM " + quotePath([]string{t.dataset, t.table})

	code, body = f.get(r, tablePath(alias.dataset, alias.table))
	switch code {
	case http.StatusOK:
		var view struct {
			Schema json.RawMessage `json:"schema"`
			View   struct {
				Query string `json:"query"`
			} `json:"view"`
		}
		if json.Unmarshal(body, &view) == nil && view.View.Query == query && sameValue(view.Schema, table.Schema) {
			return alias, true, nil
		}
		if code, body := f.send(r, http.MethodDelete, tablePath(alias.dataset, alias.table), nil); code != http.StatusOK &&
			code != http.StatusNoContent && code != 0 && code != http.StatusNotFound {
			return readTable{}, false, failed("replace its view of the table", code, body)
		}
	case http.StatusNotFound:
	default:
		return readTable{}, false, failed("read its view of the table", code, body)
	}

	insert, _ := json.Marshal(map[string]any{
		"tableReference": map[string]string{"projectId": alias.project, "datasetId": alias.dataset, "tableId": alias.table},
		"view":           map[string]any{"query": query, "useLegacySql": false},
	})
	tables := "/datasets/" + url.PathEscape(alias.dataset) + "/tables"
	code, body = f.send(r, http.MethodPost, tables, insert)
	if code == http.StatusNotFound {
		ds, _ := json.Marshal(map[string]any{"datasetReference": map[string]string{"projectId": alias.project, "datasetId": alias.dataset}})
		if code, body := f.send(r, http.MethodPost, "/datasets", ds); code != http.StatusOK && code != 0 && code != http.StatusConflict {
			return readTable{}, false, failed("make its dataset "+readAliasDataset, code, body)
		}
		code, body = f.send(r, http.MethodPost, tables, insert)
	}
	if code != http.StatusOK && code != 0 {
		return readTable{}, false, failed("make its view of the table", code, body)
	}
	if len(table.Schema) > 0 && string(table.Schema) != "null" {
		patch, _ := json.Marshal(map[string]json.RawMessage{"schema": table.Schema})
		if code, body := f.send(r, http.MethodPatch, tablePath(alias.dataset, alias.table), patch); code != http.StatusOK && code != 0 {
			return readTable{}, false, failed("give its view the table's schema", code, body)
		}
	}
	return alias, true, nil
}

// sameValue reports whether a and b are the same JSON value.
func sameValue(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// aliasRequest is req for alias instead of its table.
func aliasRequest(req *storagepb.CreateReadSessionRequest, alias readTable) (rawFrame, error) {
	out := proto.Clone(req).(*storagepb.CreateReadSessionRequest)
	if out.ReadSession == nil {
		out.ReadSession = &storagepb.ReadSession{}
	}
	out.ReadSession.Table = alias.path()
	b, err := proto.Marshal(out)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode CreateReadSessionRequest: %v", err)
	}
	return b, nil
}

// clientSession is the emulator's session of an alias as the client's of
// t: its table t's, and an Avro schema's namespace and name t's.
func clientSession(fr rawFrame, t readTable) (rawFrame, *storagepb.ReadSession) {
	var sess storagepb.ReadSession
	if proto.Unmarshal(fr, &sess) != nil {
		return fr, nil
	}
	sess.Table = t.path()
	if a := sess.GetAvroSchema(); a != nil {
		a.Schema = avroSchemaOf(a.Schema, t)
	}
	b, err := proto.Marshal(&sess)
	if err != nil {
		return fr, &sess
	}
	return b, &sess
}

// clientRows is a ReadRows answer of an alias as one of t: an Avro
// schema's namespace and name t's.
func clientRows(fr rawFrame, t readTable) rawFrame {
	var resp storagepb.ReadRowsResponse
	if proto.Unmarshal(fr, &resp) != nil {
		return fr
	}
	a := resp.GetAvroSchema()
	if a == nil {
		return fr
	}
	a.Schema = avroSchemaOf(a.Schema, t)
	b, err := proto.Marshal(&resp)
	if err != nil {
		return fr
	}
	return b
}

// avroSchemaOf is an Avro schema the emulator wrote for an alias with the
// namespace and name it writes for t (TableToAVRO: "project.dataset" and
// the table ID).
func avroSchemaOf(schema string, t readTable) string {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(schema), &m) != nil {
		return schema
	}
	m["namespace"], _ = json.Marshal(t.project + "." + t.dataset)
	m["name"], _ = json.Marshal(t.table)
	b, err := json.Marshal(m)
	if err != nil {
		return schema
	}
	return string(b)
}
