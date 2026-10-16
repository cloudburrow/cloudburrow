// Package bqiceberg writes the Apache Iceberg files of a BigQuery table for
// Apache Iceberg (#1427): the table's rows as Parquet data files, and its
// Iceberg metadata (metadata.json, manifest lists, manifests), with the
// Apache Iceberg Go library (github.com/apache/iceberg-go, Apache-2.0,
// pinned in go.mod).
//
// A Store keeps a mirror of every file it writes on its root, a directory
// of the instance's state, named by its gs:// URI; the caller uploads the
// files Write and Metadata name to Cloud Storage. Each Write is one Iceberg
// snapshot that overwrites the table's rows with the rows given: the data
// files of the earlier snapshots are kept, as Iceberg keeps them for time
// travel until snapshots are expired.
package bqiceberg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/decimal128"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/iceberg-go"
	icebergio "github.com/apache/iceberg-go/io"
	"github.com/apache/iceberg-go/table"
)

// Column is one column of a table: Type is a BigQuery type (INT64,
// FLOAT64, BOOL, STRING, BYTES, NUMERIC, DATE, DATETIME, TIME, TIMESTAMP),
// Scale a NUMERIC's (9 when unparameterized) and Precision its precision.
type Column struct {
	Name      string
	Type      string
	Required  bool
	Precision int
	Scale     int
}

// Value types a row holds, by column type: INT64 int64, FLOAT64 float64,
// BOOL bool, STRING string, BYTES []byte, NUMERIC *big.Int (unscaled),
// DATE int32 (days since the epoch), DATETIME, TIME and TIMESTAMP int64
// (microseconds; TIME since midnight). nil is NULL.

// IcebergType is the Iceberg type of a BigQuery type, as BigQuery's
// documentation maps them
// (https://cloud.google.com/bigquery/docs/iceberg-tables#data-types), or
// false when it is not one this package writes.
func IcebergType(c Column) (iceberg.Type, bool) {
	switch strings.ToUpper(c.Type) {
	case "INT64", "INTEGER":
		return iceberg.PrimitiveTypes.Int64, true
	case "FLOAT64", "FLOAT":
		return iceberg.PrimitiveTypes.Float64, true
	case "BOOL", "BOOLEAN":
		return iceberg.PrimitiveTypes.Bool, true
	case "STRING":
		return iceberg.PrimitiveTypes.String, true
	case "BYTES":
		return iceberg.PrimitiveTypes.Binary, true
	case "NUMERIC":
		p, s := c.Precision, c.Scale
		if p == 0 {
			p, s = 38, 9
		}
		return iceberg.DecimalTypeOf(p, s), true
	case "DATE":
		return iceberg.PrimitiveTypes.Date, true
	case "DATETIME":
		return iceberg.PrimitiveTypes.Timestamp, true
	case "TIME":
		return iceberg.PrimitiveTypes.Time, true
	case "TIMESTAMP":
		return iceberg.PrimitiveTypes.TimestampTz, true
	}
	return nil, false
}

// Store writes Iceberg tables, mirroring their files under Root.
type Store struct {
	Root string
	mu   sync.Mutex
}

// LocalPath is where the mirror keeps uri, a gs:// URI.
func (s *Store) LocalPath(uri string) string {
	return filepath.Join(s.Root, "files", filepath.FromSlash(strings.TrimPrefix(uri, "gs://")))
}

// pointer is the file that names location's current metadata file.
func (s *Store) pointer(location string) string {
	sum := sha256.Sum256([]byte(location))
	return filepath.Join(s.Root, "current", hex.EncodeToString(sum[:16]))
}

// Forget drops what the Store keeps of location's table (a table dropped,
// or made again of the name), so the next Write starts a new table.
// Files already written stay.
func (s *Store) Forget(location string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = os.Remove(s.pointer(location))
}

// mirror is the iceberg-go IO of the Store: gs:// names on the local
// mirror, recording what is created.
type mirror struct {
	s       *Store
	mu      sync.Mutex
	created []string
}

func (m *mirror) path(name string) (string, error) {
	if !strings.HasPrefix(name, "gs://") {
		return "", &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	return m.s.LocalPath(name), nil
}

func (m *mirror) Open(name string) (icebergio.File, error) {
	p, err := m.path(name)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

func (m *mirror) ReadFile(name string) ([]byte, error) {
	p, err := m.path(name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func (m *mirror) Remove(name string) error {
	// Iceberg-go removes a file it wrote of a failed commit; the files of
	// committed snapshots are never removed by it here.
	p, err := m.path(name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

func (m *mirror) Create(name string) (icebergio.FileWriter, error) {
	p, err := m.path(name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(p)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.created = append(m.created, name)
	m.mu.Unlock()
	return f, nil
}

func (m *mirror) WriteFile(name string, p []byte) error {
	w, err := m.Create(name)
	if err != nil {
		return err
	}
	if _, err := w.Write(p); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

// catalog is the CatalogIO of one table's commit: it applies the updates
// to the table's metadata, writes metadata/vN.metadata.json and points the
// Store at it.
type catalog struct {
	s        *Store
	io       *mirror
	location string
	current  table.Metadata
	curLoc   string
	version  int
}

func (c *catalog) LoadTable(context.Context, table.Identifier) (*table.Table, error) {
	return nil, errors.New("bqiceberg: LoadTable is not served")
}

func (c *catalog) CommitTable(_ context.Context, _ table.Identifier, reqs []table.Requirement, updates []table.Update) (table.Metadata, string, error) {
	for _, r := range reqs {
		if err := r.Validate(c.current); err != nil {
			return nil, "", err
		}
	}
	next, err := table.UpdateTableMetadata(c.current, updates, c.curLoc)
	if err != nil {
		return nil, "", err
	}
	c.version++
	loc := fmt.Sprintf("%s/metadata/v%d.metadata.json", c.location, c.version)
	w, err := c.io.Create(loc)
	if err != nil {
		return nil, "", err
	}
	if err := writeJSON(w, next); err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(filepath.Dir(c.s.pointer(c.location)), 0o755); err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(c.s.pointer(c.location), []byte(fmt.Sprintf("%d %s", c.version, loc)), 0o644); err != nil {
		return nil, "", err
	}
	c.current, c.curLoc = next, loc
	return next, loc, nil
}

// Schema is the Iceberg schema of cols, its field IDs 1 to n in order.
func Schema(cols []Column) (*iceberg.Schema, error) {
	fields := make([]iceberg.NestedField, len(cols))
	for i, c := range cols {
		t, ok := IcebergType(c)
		if !ok {
			return nil, fmt.Errorf("the column %s, of type %s, which a BigQuery table for Apache Iceberg does not hold here", c.Name, c.Type)
		}
		fields[i] = iceberg.NestedField{ID: i + 1, Name: c.Name, Type: t, Required: c.Required}
	}
	return iceberg.NewSchema(0, fields...), nil
}

// Write commits rows, the table's rows now, as a snapshot of the Iceberg
// table at location (a gs:// URI), creating the table at the first Write.
// It returns the URIs of the files it wrote: the data files, and the
// metadata files, which the caller keeps until an export (Metadata).
func (s *Store) Write(ctx context.Context, location string, cols []Column, rows [][]any) (data []string, err error) {
	location = strings.TrimSuffix(location, "/")
	sc, err := Schema(cols)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mir := &mirror{s: s}
	cat := &catalog{s: s, io: mir, location: location}
	ident := table.Identifier{"bigquery", "table"}
	fsF := func(context.Context) (icebergio.IO, error) { return mir, nil }
	var tbl *table.Table
	if b, err := os.ReadFile(s.pointer(location)); err == nil {
		var loc string
		if _, err := fmt.Sscanf(string(b), "%d %s", &cat.version, &loc); err != nil {
			return nil, fmt.Errorf("bqiceberg: the table's pointer: %w", err)
		}
		if tbl, err = table.NewFromLocation(ctx, ident, loc, fsF, cat); err != nil {
			return nil, err
		}
		cat.current, cat.curLoc = tbl.Metadata(), loc
		if !tbl.Schema().Equals(sc) {
			// A column added or dropped (ALTER TABLE): the new schema,
			// matched to the old by name, becomes current.
			if tbl, err = evolve(ctx, tbl, sc); err != nil {
				return nil, err
			}
		}
	} else {
		meta, err := table.NewMetadata(sc, iceberg.UnpartitionedSpec, table.UnsortedSortOrder, location,
			iceberg.Properties{table.PropertyFormatVersion: "2"})
		if err != nil {
			return nil, err
		}
		tbl = table.New(ident, meta, "", fsF, cat)
		cat.current = meta
		// The table's first metadata file, of no snapshot.
		if _, _, err := cat.CommitTable(ctx, ident, nil, nil); err != nil {
			return nil, err
		}
		tbl = table.New(ident, cat.current, cat.curLoc, fsF, cat)
	}
	arrowSchema, err := table.SchemaToArrowSchema(tbl.Schema(), nil, true, false)
	if err != nil {
		return nil, err
	}
	rec, err := record(arrowSchema, cols, rows)
	if err != nil {
		return nil, err
	}
	defer rec.Release()
	at := array.NewTableFromRecords(arrowSchema, []arrow.RecordBatch{rec})
	defer at.Release()
	tx := tbl.NewTransaction()
	switch {
	case len(rows) == 0 && tbl.CurrentSnapshot() == nil:
		return nil, nil // the table, of no snapshot
	case len(rows) == 0:
		err = tx.Delete(ctx, iceberg.AlwaysTrue{}, nil)
	case tbl.CurrentSnapshot() == nil:
		err = tx.AppendTable(ctx, at, 1<<20, nil)
	default:
		err = tx.OverwriteTable(ctx, at, 1<<20, nil)
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	for _, n := range mir.created {
		if strings.HasPrefix(n, location+"/data/") {
			data = append(data, n)
		}
	}
	sort.Strings(data)
	return data, nil
}

// evolve makes sc, the table's columns now, its current schema.
func evolve(ctx context.Context, tbl *table.Table, sc *iceberg.Schema) (*table.Table, error) {
	tx := tbl.NewTransaction()
	us := tx.UpdateSchema(true, false)
	old := tbl.Schema()
	for _, f := range old.Fields() {
		if _, ok := sc.FindFieldByName(f.Name); !ok {
			us = us.DeleteColumn([]string{f.Name})
		}
	}
	for _, f := range sc.Fields() {
		of, ok := old.FindFieldByName(f.Name)
		switch {
		case !ok:
			us = us.AddColumn([]string{f.Name}, f.Type, "", false, nil)
		case !of.Type.Equals(f.Type):
			return nil, fmt.Errorf("the column %s changed its type from %s to %s", f.Name, of.Type, f.Type)
		}
	}
	if err := us.Commit(); err != nil {
		return nil, err
	}
	return tx.Commit(ctx)
}

// Metadata is the URIs of the current metadata of location's table: its
// metadata.json, its current snapshot's manifest list and that list's
// manifests, the files an export writes (EXPORT TABLE METADATA). ok is
// false when there is no table.
func (s *Store) Metadata(ctx context.Context, location string) (files []string, ok bool, err error) {
	location = strings.TrimSuffix(location, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.pointer(location))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var v int
	var loc string
	if _, err := fmt.Sscanf(string(b), "%d %s", &v, &loc); err != nil {
		return nil, false, err
	}
	mir := &mirror{s: s}
	tbl, err := table.NewFromLocation(ctx, table.Identifier{"bigquery", "table"}, loc,
		func(context.Context) (icebergio.IO, error) { return mir, nil }, &catalog{})
	if err != nil {
		return nil, false, err
	}
	files = []string{loc}
	if snap := tbl.CurrentSnapshot(); snap != nil {
		files = append(files, snap.ManifestList)
		mans, err := snap.Manifests(mir)
		if err != nil {
			return nil, false, err
		}
		for _, m := range mans {
			files = append(files, m.FilePath())
		}
	}
	return files, true, nil
}

func writeJSON(w icebergio.FileWriter, meta table.Metadata) error {
	b, err := json.Marshal(meta)
	if err != nil {
		_ = w.Close()
		return err
	}
	if _, err := w.Write(b); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

// record builds rows as one Arrow record of schema.
func record(schema *arrow.Schema, cols []Column, rows [][]any) (arrow.RecordBatch, error) {
	b := array.NewRecordBuilder(memory.DefaultAllocator, schema)
	defer b.Release()
	for i, row := range rows {
		for j, c := range cols {
			var v any
			if j < len(row) {
				v = row[j]
			}
			if err := appendValue(b.Field(j), c, v); err != nil {
				return nil, fmt.Errorf("row %d, column %s: %w", i+1, c.Name, err)
			}
		}
	}
	return b.NewRecordBatch(), nil
}

func appendValue(fb array.Builder, c Column, v any) error {
	if v == nil {
		if c.Required {
			return errors.New("NULL in a REQUIRED column")
		}
		fb.AppendNull()
		return nil
	}
	bad := fmt.Errorf("a %T for %s", v, c.Type)
	switch b := fb.(type) {
	case *array.Int64Builder:
		x, ok := v.(int64)
		if !ok {
			return bad
		}
		b.Append(x)
	case *array.Float64Builder:
		x, ok := v.(float64)
		if !ok {
			return bad
		}
		b.Append(x)
	case *array.BooleanBuilder:
		x, ok := v.(bool)
		if !ok {
			return bad
		}
		b.Append(x)
	case *array.StringBuilder:
		x, ok := v.(string)
		if !ok {
			return bad
		}
		b.Append(x)
	case *array.BinaryBuilder:
		x, ok := v.([]byte)
		if !ok {
			return bad
		}
		b.Append(x)
	case *array.Decimal128Builder:
		x, ok := v.(*big.Int)
		if !ok {
			return bad
		}
		b.Append(decimal128.FromBigInt(x))
	case *array.Date32Builder:
		x, ok := v.(int32)
		if !ok {
			return bad
		}
		b.Append(arrow.Date32(x))
	case *array.Time64Builder:
		x, ok := v.(int64)
		if !ok {
			return bad
		}
		b.Append(arrow.Time64(x))
	case *array.TimestampBuilder:
		x, ok := v.(int64)
		if !ok {
			return bad
		}
		b.Append(arrow.Timestamp(x))
	default:
		return fmt.Errorf("no Arrow builder for %s", c.Type)
	}
	return nil
}

// ReadAll is every row of the Iceberg table whose metadata file is
// metadataURI, read with iceberg-go from the files under root (a
// directory named as LocalPath names them): a reader's view of what was
// written, for tests.
func ReadAll(ctx context.Context, root, metadataURI string) (arrow.Table, error) {
	mir := &mirror{s: &Store{Root: root}}
	tbl, err := table.NewFromLocation(ctx, table.Identifier{"bigquery", "table"}, metadataURI,
		func(context.Context) (icebergio.IO, error) { return mir, nil }, &catalog{})
	if err != nil {
		return nil, err
	}
	return tbl.Scan().ToArrowTable(ctx)
}
