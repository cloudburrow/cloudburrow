package bigqueryfront

// Change data capture through the Storage Write API (#1999), to
// https://docs.cloud.google.com/bigquery/docs/change-data-capture:
//
//   - a writer schema with the pseudo-column _CHANGE_TYPE (a STRING) makes
//     each row a change: "The pseudocolumn _CHANGE_TYPE only accepts the
//     values UPSERT and DELETE"; any other value, or none, is the row's
//     error, "The given value is not a valid CHANGE_TYPE";
//   - _CHANGE_SEQUENCE_NUMBER (a STRING, optional) orders the changes of a
//     key: hexadecimal sections separated by "/", "at most 16 hexadecimal
//     characters" each, "up to four sections", either case, compared
//     section by section "as unsigned numeric values"; a change whose
//     number is below the highest the key has had has no effect, and of
//     two equal numbers the later ingested wins; a change without a number
//     is applied in the order it arrives ("unpredictable ordering" in
//     BigQuery when numbered and unnumbered changes are mixed);
//   - "You must use the Storage Write API (gRPC) in the default stream" and
//     "the protobuf format": a change to another stream, or Arrow rows
//     with the pseudo-columns, is INVALID_ARGUMENT;
//   - "You must declare primary keys for the destination table": a table
//     without one is INVALID_ARGUMENT, as is a key of more than 16 columns
//     ("Composite primary keys containing up to 16 columns are supported")
//     and a table of more than 2,000 top-level columns;
//   - the pseudo-columns "are not queryable columns": they are never
//     written to the table.
//
// An UPSERT replaces the row of its primary key, or inserts it; a DELETE
// removes it. BigQuery applies changes in the background, "at least once
// within the interval defined by the max_staleness value", and a query
// past that interval merges the pending changes at run time; CloudBurrow
// applies each append's changes before it answers it, so every query reads
// them, which is what a query of BigQuery's that merges at run time
// reads. A DELETE's key is remembered two days ("BigQuery maintains a
// delete retention window of two days"), so a late change with a lower
// number does not bring the row back; an UPSERT's key as long as the row.
//
// The changes are carried out on the REST front, as the front's own
// statements (asInternal, kept out of the job listings): a DELETE of the
// changed keys' rows, then tabledata.insertAll of the upserted rows. The
// streaming buffer's DML check (dml.go) does not apply to them, as BigQuery
// applies changes to rows just streamed. The highest sequence number of
// each key is kept in the front, and in --state-dir when it has one.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	changeTypeColumn = "_CHANGE_TYPE"
	changeSeqColumn  = "_CHANGE_SEQUENCE_NUMBER"
	// cdcMaxKeyColumns and cdcMaxColumns are BigQuery's documented limits.
	cdcMaxKeyColumns = 16
	cdcMaxColumns    = 2000
	// cdcDeleteRetention is how long a deleted key's number is kept.
	cdcDeleteRetention = 48 * time.Hour
	// cdcKeysPerDelete bounds the keys one DELETE statement names.
	cdcKeysPerDelete = 200
)

// changePseudoColumn is the pseudo-column a top-level writer field named
// name is, or "".
func changePseudoColumn(name string) string {
	switch {
	case strings.EqualFold(name, changeTypeColumn):
		return changeTypeColumn
	case strings.EqualFold(name, changeSeqColumn):
		return changeSeqColumn
	}
	return ""
}

// takePseudoColumn records fd as the pseudo-column pc: a STRING field.
func (rc *rowConv) takePseudoColumn(pc string, fd protoreflect.FieldDescriptor) error {
	if fd.Kind() != protoreflect.StringKind || fd.IsList() {
		return status.Errorf(codes.InvalidArgument, "Invalid proto schema: the pseudo-column %s must be a non-repeated string field", pc)
	}
	if pc == changeTypeColumn {
		rc.changeType = fd
	} else {
		rc.changeSeq = fd
	}
	return nil
}

// cdc reports whether the writer schema writes changes.
func (rc *rowConv) cdc() bool { return rc.changeType != nil || rc.changeSeq != nil }

// rowChange is a row's pseudo-columns.
type rowChange struct {
	typ    string
	seq    string
	hasSeq bool
}

// changeSeq is a parsed _CHANGE_SEQUENCE_NUMBER.
type changeSeq []uint64

// parseChangeSeq reads s in BigQuery's format.
func parseChangeSeq(s string) (changeSeq, error) {
	parts := strings.Split(s, "/")
	if s == "" || len(parts) > 4 {
		return nil, fmt.Errorf("the %s %q is not up to four hexadecimal sections separated by '/'", changeSeqColumn, s)
	}
	out := make(changeSeq, len(parts))
	for i, p := range parts {
		if p == "" || len(p) > 16 {
			return nil, fmt.Errorf("the %s %q has a section that is not 1 to 16 hexadecimal characters", changeSeqColumn, s)
		}
		v, err := strconv.ParseUint(p, 16, 64)
		if err != nil {
			return nil, fmt.Errorf("the %s %q has a section that is not hexadecimal", changeSeqColumn, s)
		}
		out[i] = v
	}
	return out, nil
}

// compare is -1, 0 or 1 as a is before, equal to or after b: section by
// section, a missing section before any.
func (a changeSeq) compare(b changeSeq) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// cdcTable is what a change append needs of its table.
type cdcTable struct {
	key     []string
	created string
	err     error
}

// changeTable reads the table's primary key, refusing a table change data
// capture cannot write.
func (w *storageWrite) changeTable(ctx context.Context, t readTable) cdcTable {
	code, body := w.call(ctx, http.MethodGet, t, "", nil)
	if code != http.StatusOK {
		return cdcTable{err: restStatus(code, body, "the table "+t.path())}
	}
	var meta struct {
		CreationTime     string            `json:"creationTime"`
		Schema           tableSchema       `json:"schema"`
		TableConstraints *tableConstraints `json:"tableConstraints"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return cdcTable{err: status.Errorf(codes.Internal, "read the table %s: %v", t.path(), err)}
	}
	if meta.TableConstraints == nil || meta.TableConstraints.PrimaryKey == nil || len(meta.TableConstraints.PrimaryKey.Columns) == 0 {
		return cdcTable{err: status.Errorf(codes.InvalidArgument,
			"The table %s has no primary key: change data capture (%s) requires the destination table to declare a primary key", t.path(), changeTypeColumn)}
	}
	key := meta.TableConstraints.PrimaryKey.Columns
	if len(key) > cdcMaxKeyColumns {
		return cdcTable{err: status.Errorf(codes.InvalidArgument,
			"The primary key of %s has %d columns: change data capture supports composite primary keys of up to %d columns", t.path(), len(key), cdcMaxKeyColumns)}
	}
	if n := len(meta.Schema.Fields); n > cdcMaxColumns {
		return cdcTable{err: status.Errorf(codes.InvalidArgument,
			"The table %s has %d top-level columns: a table with change data capture cannot have more than %d", t.path(), n, cdcMaxColumns)}
	}
	return cdcTable{key: key, created: meta.CreationTime}
}

// cdcKeys is the highest sequence number each key of one table has had.
type cdcKeys struct {
	Created string             `json:"created"`
	Keys    map[string]cdcSeen `json:"keys"`
}

type cdcSeen struct {
	Seq changeSeq `json:"seq,omitempty"`
	// Deleted is when the key's last change, a DELETE, was applied.
	Deleted time.Time `json:"deleted,omitzero"`
}

// cdcState is the front's change data capture state, by table.
type cdcState struct {
	mu     sync.Mutex
	tables map[string]*sync.Mutex
}

func (s *cdcState) lock(table string) func() {
	s.mu.Lock()
	if s.tables == nil {
		s.tables = map[string]*sync.Mutex{}
	}
	m := s.tables[table]
	if m == nil {
		m = &sync.Mutex{}
		s.tables[table] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func (w *storageWrite) cdcFile(t readTable) string {
	dir := w.stateDir()
	if dir == "" {
		return ""
	}
	h := sha256.Sum256([]byte(t.path()))
	return filepath.Join(dir, "cdc-"+hex.EncodeToString(h[:12])+".json")
}

// loadKeys is the table's keys, made anew when the table is not the one
// they were kept for (deleted and created again).
func (w *storageWrite) loadKeys(t readTable, created string) *cdcKeys {
	w.mu.Lock()
	k := w.cdcKeys[t.path()]
	w.mu.Unlock()
	if k == nil {
		if f := w.cdcFile(t); f != "" {
			if b, err := os.ReadFile(f); err == nil {
				var kept cdcKeys
				if json.Unmarshal(b, &kept) == nil {
					k = &kept
				}
			}
		}
	}
	if k == nil || k.Created != created || k.Keys == nil {
		k = &cdcKeys{Created: created, Keys: map[string]cdcSeen{}}
	}
	now := w.now()
	for key, s := range k.Keys {
		if !s.Deleted.IsZero() && now.Sub(s.Deleted) > cdcDeleteRetention {
			delete(k.Keys, key)
		}
	}
	w.mu.Lock()
	if w.cdcKeys == nil {
		w.cdcKeys = map[string]*cdcKeys{}
	}
	w.cdcKeys[t.path()] = k
	w.mu.Unlock()
	return k
}

func (w *storageWrite) saveKeys(t readTable, k *cdcKeys) {
	f := w.cdcFile(t)
	if f == "" {
		return
	}
	b, err := json.Marshal(k)
	if err == nil {
		err = writeFileAtomic(f, b)
	}
	if err != nil {
		w.logf("bigquery front: keeping the change sequence numbers of %s: %v", t.path(), err)
	}
}

// change is one row's change, checked.
type change struct {
	upsert bool
	key    string
	keyVal []any
	seq    changeSeq
	raw    json.RawMessage
}

// appendChanges is appendOne for a writer schema with the pseudo-columns:
// it checks and applies the request's changes.
func (w *storageWrite) appendChanges(ctx context.Context, name string, st *writeStream, cr convResult, req *storagepb.AppendRowsRequest,
	missing *missingValues) *storagepb.AppendRowsResponse {
	if !st.isDef {
		return appendFailed(name, status.Errorf(codes.InvalidArgument,
			"Change data capture (%s) is supported only on the _default stream: %s", changeTypeColumn, name))
	}
	if req.GetOffset() != nil {
		return appendFailed(name, status.Errorf(codes.InvalidArgument, "an offset cannot be given for the _default stream: %s", name))
	}
	if cr.conv.changeType == nil {
		return appendFailed(name, status.Errorf(codes.InvalidArgument,
			"The writer schema has %s and no %s: change data capture requires %s", changeSeqColumn, changeTypeColumn, changeTypeColumn))
	}
	tbl := w.changeTable(ctx, st.table)
	if tbl.err != nil {
		return appendFailed(name, tbl.err)
	}
	keyCols := make([]field, len(tbl.key))
	for i, k := range tbl.key {
		c, ok := fieldNamed(cr.fields, k)
		if !ok {
			return appendFailed(name, status.Errorf(codes.Internal, "the primary key column %s is not in the table %s", k, st.table.path()))
		}
		if strings.EqualFold(c.Mode, "REPEATED") || legacyType(c.Type) == "RECORD" || legacyType(c.Type) == "RANGE" {
			return appendFailed(name, status.Errorf(codes.InvalidArgument, "the primary key column %s of %s is a %s", k, st.table.path(), c.Type))
		}
		keyCols[i] = c
	}

	serialized := req.GetProtoRows().GetRows().GetSerializedRows()
	changes := make([]change, 0, len(serialized))
	var rowErrs []*storagepb.RowError
	for i, b := range serialized {
		obj, ch, err := cr.conv.rowChange(b)
		if err != nil {
			rowErrs = append(rowErrs, fieldsError(i, "%v", err))
			continue
		}
		var c change
		switch ch.typ {
		case "UPSERT":
			c.upsert = true
		case "DELETE":
		default:
			rowErrs = append(rowErrs, fieldsError(i, "The given value is not a valid CHANGE_TYPE: %q", ch.typ))
			continue
		}
		if ch.hasSeq {
			if c.seq, err = parseChangeSeq(ch.seq); err != nil {
				rowErrs = append(rowErrs, fieldsError(i, "%v", err))
				continue
			}
		}
		if c.upsert {
			raw, rowErr, err := checkedRow(cr.fields, missing.apply(cr.fields, obj), i)
			if err != nil {
				return appendFailed(name, err)
			}
			if rowErr != nil {
				rowErrs = append(rowErrs, rowErr)
				continue
			}
			c.raw = raw
		}
		for _, kc := range keyCols {
			v, _ := valueNamed(obj, kc.Name)
			c.keyVal = append(c.keyVal, v)
		}
		kb, _ := json.Marshal(c.keyVal)
		c.key = string(kb)
		changes = append(changes, c)
	}
	if len(rowErrs) > 0 {
		resp := appendFailed(name, status.Error(codes.InvalidArgument,
			"Errors found while processing rows. Please refer to the row_errors field for details. The list may not be complete "+
				"because of the size limitations. Entity: "+name))
		resp.RowErrors = rowErrs
		return resp
	}

	if err := w.applyChanges(ctx, st.table, tbl, keyCols, changes); err != nil {
		return appendFailed(name, err)
	}
	st.mu.Lock()
	st.rows += int64(len(serialized))
	if err := w.saveStream(st); err != nil {
		w.logf("bigquery front: keeping the offset of %s: %v", name, err)
	}
	st.mu.Unlock()
	return &storagepb.AppendRowsResponse{WriteStream: name,
		Response: &storagepb.AppendRowsResponse_AppendResult_{AppendResult: &storagepb.AppendRowsResponse_AppendResult{}}}
}

// valueNamed is obj's value of the column name, matched without case.
func valueNamed(obj map[string]any, name string) (any, bool) {
	if v, ok := obj[name]; ok {
		return v, true
	}
	for k, v := range obj {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return nil, false
}

// applyChanges applies the changes, in order, to the table.
func (w *storageWrite) applyChanges(ctx context.Context, t readTable, tbl cdcTable, keyCols []field, changes []change) error {
	unlock := w.cdc.lock(t.path())
	defer unlock()
	keys := w.loadKeys(t, tbl.created)
	now := w.now()
	final := map[string]change{}
	var order []string
	seen := map[string]cdcSeen{}
	for _, c := range changes {
		prev, ok := seen[c.key]
		if !ok {
			prev, ok = keys.Keys[c.key]
		}
		if ok && c.seq != nil && prev.Seq != nil && c.seq.compare(prev.Seq) < 0 {
			continue // "don't have any effect on the row"
		}
		next := cdcSeen{Seq: c.seq}
		if c.seq == nil {
			next.Seq = prev.Seq
		}
		if !c.upsert {
			next.Deleted = now
		}
		seen[c.key] = next
		if _, ok := final[c.key]; !ok {
			order = append(order, c.key)
		}
		final[c.key] = c
	}
	if len(order) == 0 {
		return nil
	}
	ctx = asInternal(context.WithValue(ctx, changeApplyKey{}, true))
	for start := 0; start < len(order); start += cdcKeysPerDelete {
		end := min(start+cdcKeysPerDelete, len(order))
		if err := w.deleteKeys(ctx, t, keyCols, order[start:end], final); err != nil {
			return err
		}
	}
	var rows []json.RawMessage
	for _, k := range order {
		if c := final[k]; c.upsert {
			rows = append(rows, c.raw)
		}
	}
	if len(rows) > 0 {
		if err := w.insert(ctx, t, rows); err != nil {
			return err
		}
	}
	for k, s := range seen {
		keys.Keys[k] = s
	}
	w.saveKeys(t, keys)
	return nil
}

// changeApplyKey marks the front's own statements that apply changes,
// which the streaming buffer's DML check lets through (dml.go).
type changeApplyKey struct{}

func isChangeApply(ctx context.Context) bool {
	v, _ := ctx.Value(changeApplyKey{}).(bool)
	return v
}

// deleteKeys deletes the rows of the keys through jobs.query, each key's
// values as query parameters.
func (w *storageWrite) deleteKeys(ctx context.Context, t readTable, keyCols []field, keys []string, final map[string]change) error {
	type paramType struct {
		Type string `json:"type"`
	}
	type paramValue struct {
		Value string `json:"value"`
	}
	type param struct {
		Name  string     `json:"name"`
		Type  paramType  `json:"parameterType"`
		Value paramValue `json:"parameterValue"`
	}
	var conds []string
	var params []param
	for i, k := range keys {
		vals := final[k].keyVal
		var parts []string
		for j, kc := range keyCols {
			col := "`" + strings.ReplaceAll(kc.Name, "`", "\\`") + "`"
			v := vals[j]
			if v == nil {
				parts = append(parts, col+" IS NULL")
				continue
			}
			pn := fmt.Sprintf("k%d_%d", i, j)
			parts = append(parts, col+" = @"+pn)
			params = append(params, param{Name: pn, Type: paramType{cdcParamType(kc.Type)}, Value: paramValue{paramText(v)}})
		}
		conds = append(conds, "("+strings.Join(parts, " AND ")+")")
	}
	legacy := false
	body, err := json.Marshal(map[string]any{
		"query":           "DELETE FROM `" + t.project + "." + t.dataset + "." + t.table + "` WHERE " + strings.Join(conds, " OR "),
		"useLegacySql":    &legacy,
		"parameterMode":   "NAMED",
		"queryParameters": params,
	})
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	p := "/bigquery/v2/projects/" + t.project + "/queries"
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://bigquery"+p, strings.NewReader(string(body)))
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	r.Header.Set("Content-Type", "application/json")
	rec := newRecorder()
	w.rest.ServeHTTP(rec, r)
	if rec.status != 0 && rec.status != http.StatusOK {
		return restStatus(rec.status, rec.body.Bytes(), "applying the changes to "+t.path())
	}
	return nil
}

// cdcParamType is a column type's GoogleSQL name, for a query parameter.
func cdcParamType(t string) string {
	switch legacyType(t) {
	case "INTEGER":
		return "INT64"
	case "FLOAT":
		return "FLOAT64"
	case "BOOLEAN":
		return "BOOL"
	}
	return strings.ToUpper(t)
}

// paramText is a row value as a query parameter's value.
func paramText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	}
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}
