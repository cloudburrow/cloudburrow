package firestoreadmin

// Exports, imports and bulk deletes (#1500). The front reads the
// database's documents from the emulator over its own gRPC API
// (ListCollectionIds and ListDocuments, recursively, with missing parents),
// and writes them back with Commit, so they work on whatever the emulator
// holds.
//
// An export is written to the instance's Cloud Storage (the --storage the
// call front is given) in the layout Google documents for a managed
// export: under the output prefix, an `<prefix base>.overall_export_metadata`
// file, and for each kind folder (`all_namespaces/all_kinds`, or
// `all_namespaces/kind_<collection id>` when collection_ids are named) an
// `<folder>.export_metadata` file and `output-N` files. The output files
// are LevelDB log files, as Google's are; their records are Firestore
// Document messages, not the legacy Datastore EntityProto Google writes,
// so an export imports back here, but BigQuery does not load it.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SetStorage names the instance's Cloud Storage (a base URL, such as
// http://storage:4443), which exports are written to and imports read
// from; without it, they are refused.
func (a *Admin) SetStorage(base string) { a.storage = strings.TrimSuffix(base, "/") }

// asOwner is ctx calling the emulator as its owner: its metadata calls
// (ListCollectionIds) require admin authentication, which the Firestore
// emulator grants the bearer token "owner".
func asOwner(ctx context.Context) context.Context {
	return metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer owner"))
}

// docs is the emulator's documents API, dialled once.
func (a *Admin) docs() (firestorepb.FirestoreClient, error) {
	a.connOnce.Do(func() {
		if a.emulator == "" {
			a.connErr = status.Error(codes.Unavailable, "no emulator to read the documents from")
			return
		}
		a.conn, a.connErr = grpc.NewClient(a.emulator, grpc.WithTransportCredentials(insecure.NewCredentials()))
	})
	if a.connErr != nil {
		return nil, a.connErr
	}
	return firestorepb.NewFirestoreClient(a.conn), nil
}

// readAll is every document of the database dbName, in name order.
func (a *Admin) readAll(ctx context.Context, dbName string) ([]*firestorepb.Document, error) {
	ctx = asOwner(ctx)
	c, err := a.docs()
	if err != nil {
		return nil, err
	}
	var out []*firestorepb.Document
	var walk func(parent string) error
	walk = func(parent string) error {
		var ids []string
		tok := ""
		for {
			r, err := c.ListCollectionIds(ctx, &firestorepb.ListCollectionIdsRequest{Parent: parent, PageToken: tok, PageSize: 300})
			if err != nil {
				return status.Errorf(status.Code(err), "listing the collections of %s: %v", parent, status.Convert(err).Message())
			}
			ids = append(ids, r.CollectionIds...)
			if tok = r.NextPageToken; tok == "" {
				break
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			tok := ""
			for {
				r, err := c.ListDocuments(ctx, &firestorepb.ListDocumentsRequest{Parent: parent, CollectionId: id,
					PageSize: 300, PageToken: tok, ShowMissing: true})
				if err != nil {
					return status.Errorf(status.Code(err), "listing %s/%s: %v", parent, id, status.Convert(err).Message())
				}
				for _, d := range r.Documents {
					// A missing document (one with subcollections alone)
					// has no create time; it is walked, not exported.
					if d.CreateTime != nil {
						out = append(out, d)
					}
					if err := walk(d.Name); err != nil {
						return err
					}
				}
				if tok = r.NextPageToken; tok == "" {
					break
				}
			}
		}
		return nil
	}
	if err := walk(dbName + "/documents"); err != nil {
		return nil, err
	}
	return out, nil
}

// collectionOf is the ID of the collection a document is in.
func collectionOf(docName string) string {
	parts := strings.Split(docName, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2]
}

// keepCollections filters docs to those in the collection groups ids
// (all of them when ids is empty).
func keepCollections(docs []*firestorepb.Document, ids []string) []*firestorepb.Document {
	if len(ids) == 0 {
		return docs
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []*firestorepb.Document
	for _, d := range docs {
		if want[collectionOf(d.Name)] {
			out = append(out, d)
		}
	}
	return out
}

// writeAll writes docs into the database dbName, renaming them from the
// database they were read from, in Commits of at most 500 writes.
func (a *Admin) writeAll(ctx context.Context, dbName string, docs []*firestorepb.Document) error {
	ctx = asOwner(ctx)
	c, err := a.docs()
	if err != nil {
		return err
	}
	for start := 0; start < len(docs); start += 500 {
		end := min(start+500, len(docs))
		req := &firestorepb.CommitRequest{Database: dbName}
		for _, d := range docs[start:end] {
			i := strings.Index(d.Name, "/documents/")
			if i < 0 {
				return status.Errorf(codes.InvalidArgument, "%q is not a document name", d.Name)
			}
			req.Writes = append(req.Writes, &firestorepb.Write{Operation: &firestorepb.Write_Update{
				Update: &firestorepb.Document{Name: dbName + d.Name[i:], Fields: d.Fields}}})
		}
		if _, err := c.Commit(ctx, req); err != nil {
			return status.Errorf(status.Code(err), "writing the documents of %s: %v", dbName, status.Convert(err).Message())
		}
	}
	return nil
}

// deleteAll deletes docs, in Commits of at most 500 writes.
func (a *Admin) deleteAll(ctx context.Context, dbName string, docs []*firestorepb.Document) error {
	ctx = asOwner(ctx)
	c, err := a.docs()
	if err != nil {
		return err
	}
	for start := 0; start < len(docs); start += 500 {
		end := min(start+500, len(docs))
		req := &firestorepb.CommitRequest{Database: dbName}
		for _, d := range docs[start:end] {
			req.Writes = append(req.Writes, &firestorepb.Write{Operation: &firestorepb.Write_Delete{Delete: d.Name}})
		}
		if _, err := c.Commit(ctx, req); err != nil {
			return status.Errorf(status.Code(err), "deleting documents of %s: %v", dbName, status.Convert(err).Message())
		}
	}
	return nil
}

func docBytes(docs []*firestorepb.Document) int64 {
	var n int64
	for _, d := range docs {
		n += int64(proto.Size(d))
	}
	return n
}

// liveDatabase checks the database exists, and returns its name.
func (a *Admin) liveDatabase(name string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	db, err := a.database(name)
	if err != nil {
		return "", err
	}
	return db.Name, nil
}

// finish records a finished operation, taking a.mu.
func (a *Admin) finish(dbName string, meta, resp proto.Message) (*longrunningpb.Operation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.done(dbName, meta, resp)
}

// --- export and import ---

var gsURIRe = regexp.MustCompile(`^gs://([a-z0-9][a-z0-9._-]{1,220}[a-z0-9])(?:/(.*))?$`)

// splitGS is a gs:// URI's bucket and object path (without a trailing /).
func splitGS(uri string) (bucket, prefix string, err error) {
	m := gsURIRe.FindStringSubmatch(uri)
	if m == nil {
		return "", "", status.Errorf(codes.InvalidArgument, "%q is not a Cloud Storage URI: want gs://BUCKET_NAME[/NAMESPACE_PATH]", uri)
	}
	return m[1], strings.TrimSuffix(m[2], "/"), nil
}

func (a *Admin) needStorage(method string) error {
	if a.storage == "" {
		return refuse(method, "is not served here: Cloud Storage is not enabled on this instance, and exports and imports go through the instance's own Cloud Storage. Start the instance with storage in --services")
	}
	return nil
}

func (a *Admin) ExportDocuments(ctx context.Context, req *adminpb.ExportDocumentsRequest) (*longrunningpb.Operation, error) {
	dbName, err := a.liveDatabase(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := a.needStorage("ExportDocuments"); err != nil {
		return nil, err
	}
	if req.GetOutputUriPrefix() == "" {
		return nil, status.Error(codes.InvalidArgument, "output_uri_prefix is required: gs://BUCKET_NAME[/NAMESPACE_PATH].")
	}
	if err := checkNamespaces(req.GetNamespaceIds()); err != nil {
		return nil, err
	}
	bucket, prefix, err := splitGS(req.GetOutputUriPrefix())
	if err != nil {
		return nil, err
	}
	start := a.now()
	if st := req.GetSnapshotTime(); st != nil {
		return nil, status.Error(codes.Unimplemented, "snapshot_time is not served here: the emulator keeps no earlier versions of the documents, so an export is of the documents as they are now")
	}
	if prefix == "" {
		// Google names the export after its start time.
		prefix = start.Format("2006-01-02T15:04:05") + fmt.Sprintf("_%05d", start.Nanosecond()/10000%100000)
	}
	gcs := &gcsClient{base: a.storage, http: a.client}
	if err := gcs.bucketExists(ctx, bucket); err != nil {
		return nil, err
	}
	all, err := a.readAll(ctx, dbName)
	if err != nil {
		return nil, err
	}
	docs := keepCollections(all, req.GetCollectionIds())
	folders := map[string][]*firestorepb.Document{}
	if len(req.GetCollectionIds()) == 0 {
		folders["all_namespaces/all_kinds"] = docs
	} else {
		for _, id := range req.GetCollectionIds() {
			folders["all_namespaces/kind_"+id] = keepCollections(docs, []string{id})
		}
	}
	for folder, ds := range folders {
		var log bytes.Buffer
		w := &logWriter{w: &log}
		for _, d := range ds {
			b, err := proto.Marshal(d)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "encoding %s: %v", d.Name, err)
			}
			w.record(b)
		}
		if err := gcs.put(ctx, bucket, prefix+"/"+folder+"/output-0", log.Bytes()); err != nil {
			return nil, err
		}
		meta, _ := json.Marshal(map[string]any{"kind": path.Base(folder), "documents": len(ds), "outputs": []string{"output-0"}})
		if err := gcs.put(ctx, bucket, prefix+"/"+folder+"/"+path.Base(folder)+".export_metadata", meta); err != nil {
			return nil, err
		}
	}
	outURI := "gs://" + bucket + "/" + prefix
	end := a.now()
	md := &adminpb.ExportDocumentsMetadata{StartTime: timestamppb.New(start), EndTime: timestamppb.New(end),
		OperationState:    adminpb.OperationState_SUCCESSFUL,
		ProgressDocuments: &adminpb.Progress{EstimatedWork: int64(len(docs)), CompletedWork: int64(len(docs))},
		ProgressBytes:     &adminpb.Progress{EstimatedWork: docBytes(docs), CompletedWork: docBytes(docs)},
		CollectionIds:     req.GetCollectionIds(), NamespaceIds: req.GetNamespaceIds(), OutputUriPrefix: outURI,
		SnapshotTime: timestamppb.New(start)}
	overall, _ := proto.Marshal(md)
	if err := gcs.put(ctx, bucket, prefix+"/"+path.Base(prefix)+".overall_export_metadata", overall); err != nil {
		return nil, err
	}
	return a.finish(dbName, md, &adminpb.ExportDocumentsResponse{OutputUriPrefix: outURI})
}

func (a *Admin) ImportDocuments(ctx context.Context, req *adminpb.ImportDocumentsRequest) (*longrunningpb.Operation, error) {
	dbName, err := a.liveDatabase(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := a.needStorage("ImportDocuments"); err != nil {
		return nil, err
	}
	if req.GetInputUriPrefix() == "" {
		return nil, status.Error(codes.InvalidArgument, "input_uri_prefix is required: the output_uri_prefix of an export.")
	}
	if err := checkNamespaces(req.GetNamespaceIds()); err != nil {
		return nil, err
	}
	bucket, prefix, err := splitGS(req.GetInputUriPrefix())
	if err != nil {
		return nil, err
	}
	if prefix == "" {
		return nil, status.Errorf(codes.InvalidArgument, "input_uri_prefix %q names a bucket alone: name the export's folder in it", req.GetInputUriPrefix())
	}
	start := a.now()
	gcs := &gcsClient{base: a.storage, http: a.client}
	if _, err := gcs.get(ctx, bucket, prefix+"/"+path.Base(prefix)+".overall_export_metadata"); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%s is not an export: it has no %s.overall_export_metadata file (%v)", req.GetInputUriPrefix(), path.Base(prefix), status.Convert(err).Message())
	}
	names, err := gcs.list(ctx, bucket, prefix+"/")
	if err != nil {
		return nil, err
	}
	var docs []*firestorepb.Document
	seen := map[string]bool{}
	for _, n := range names {
		if !outputRe.MatchString(n) {
			continue
		}
		b, err := gcs.get(ctx, bucket, n)
		if err != nil {
			return nil, err
		}
		recs, err := readLog(b)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "gs://%s/%s is not an export output file: %v", bucket, n, err)
		}
		for _, r := range recs {
			d := &firestorepb.Document{}
			if err := proto.Unmarshal(r, d); err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "gs://%s/%s holds a record that is not a document: %v", bucket, n, err)
			}
			if !seen[d.Name] {
				seen[d.Name] = true
				docs = append(docs, d)
			}
		}
	}
	docs = keepCollections(docs, req.GetCollectionIds())
	if err := a.writeAll(ctx, dbName, docs); err != nil {
		return nil, err
	}
	md := &adminpb.ImportDocumentsMetadata{StartTime: timestamppb.New(start), EndTime: timestamppb.New(a.now()),
		OperationState:    adminpb.OperationState_SUCCESSFUL,
		ProgressDocuments: &adminpb.Progress{EstimatedWork: int64(len(docs)), CompletedWork: int64(len(docs))},
		ProgressBytes:     &adminpb.Progress{EstimatedWork: docBytes(docs), CompletedWork: docBytes(docs)},
		CollectionIds:     req.GetCollectionIds(), InputUriPrefix: req.GetInputUriPrefix(), NamespaceIds: req.GetNamespaceIds()}
	return a.finish(dbName, md, &emptypb.Empty{})
}

var outputRe = regexp.MustCompile(`/output-\d+$`)

// checkNamespaces refuses namespaces other than the default: a Firestore
// Native database has none.
func checkNamespaces(ids []string) error {
	for _, id := range ids {
		if id != "" {
			return status.Errorf(codes.InvalidArgument, "namespace_ids %q: a Firestore Native mode database has the default namespace (\"\") alone", id)
		}
	}
	return nil
}

// --- bulk delete ---

func (a *Admin) BulkDeleteDocuments(ctx context.Context, req *adminpb.BulkDeleteDocumentsRequest) (*longrunningpb.Operation, error) {
	dbName, err := a.liveDatabase(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := checkNamespaces(req.GetNamespaceIds()); err != nil {
		return nil, err
	}
	start := a.now()
	all, err := a.readAll(ctx, dbName)
	if err != nil {
		return nil, err
	}
	docs := keepCollections(all, req.GetCollectionIds())
	if err := a.deleteAll(ctx, dbName, docs); err != nil {
		return nil, err
	}
	md := &adminpb.BulkDeleteDocumentsMetadata{StartTime: timestamppb.New(start), EndTime: timestamppb.New(a.now()),
		OperationState:    adminpb.OperationState_SUCCESSFUL,
		ProgressDocuments: &adminpb.Progress{EstimatedWork: int64(len(docs)), CompletedWork: int64(len(docs))},
		ProgressBytes:     &adminpb.Progress{EstimatedWork: docBytes(docs), CompletedWork: docBytes(docs)},
		CollectionIds:     req.GetCollectionIds(), NamespaceIds: req.GetNamespaceIds(), SnapshotTime: timestamppb.New(start)}
	return a.finish(dbName, md, &adminpb.BulkDeleteDocumentsResponse{})
}

// --- Cloud Storage, over its JSON API ---

type gcsClient struct {
	base string
	http *http.Client
}

func (g *gcsClient) do(ctx context.Context, method, u string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+u, rd)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Cloud Storage request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "the instance's Cloud Storage did not answer: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, status.Errorf(codes.NotFound, "Cloud Storage answered 404 for %s", u)
	case resp.StatusCode >= 300:
		return nil, status.Errorf(codes.Unavailable, "Cloud Storage answered %s for %s", resp.Status, u)
	}
	return b, nil
}

func (g *gcsClient) bucketExists(ctx context.Context, bucket string) error {
	if _, err := g.do(ctx, http.MethodGet, "/storage/v1/b/"+url.PathEscape(bucket), nil); err != nil {
		if status.Code(err) == codes.NotFound {
			return status.Errorf(codes.FailedPrecondition, "Bucket %s does not exist.", bucket)
		}
		return err
	}
	return nil
}

func (g *gcsClient) put(ctx context.Context, bucket, name string, data []byte) error {
	_, err := g.do(ctx, http.MethodPost, "/upload/storage/v1/b/"+url.PathEscape(bucket)+"/o?uploadType=media&name="+url.QueryEscape(name), data)
	return err
}

func (g *gcsClient) get(ctx context.Context, bucket, name string) ([]byte, error) {
	return g.do(ctx, http.MethodGet, "/storage/v1/b/"+url.PathEscape(bucket)+"/o/"+url.PathEscape(name)+"?alt=media", nil)
}

func (g *gcsClient) list(ctx context.Context, bucket, prefix string) ([]string, error) {
	var names []string
	tok := ""
	for {
		q := url.Values{"prefix": {prefix}}
		if tok != "" {
			q.Set("pageToken", tok)
		}
		b, err := g.do(ctx, http.MethodGet, "/storage/v1/b/"+url.PathEscape(bucket)+"/o?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return nil, status.Errorf(codes.Unavailable, "Cloud Storage listing: %v", err)
		}
		for _, it := range page.Items {
			names = append(names, it.Name)
		}
		if tok = page.NextPageToken; tok == "" {
			return names, nil
		}
	}
}

// --- LevelDB log files, the format of a managed export's output files ---

const (
	logBlock  = 32768
	logHeader = 7
	recFull   = 1
	recFirst  = 2
	recMiddle = 3
	recLast   = 4
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func maskCRC(c uint32) uint32 { return (c>>15 | c<<17) + 0xa282ead8 }

type logWriter struct {
	w   *bytes.Buffer
	off int // in the current block
}

func (l *logWriter) record(data []byte) {
	first := true
	for {
		if left := logBlock - l.off; left < logHeader {
			l.w.Write(make([]byte, left))
			l.off = 0
		}
		avail := logBlock - l.off - logHeader
		n := min(avail, len(data))
		last := n == len(data)
		t := byte(recMiddle)
		switch {
		case first && last:
			t = recFull
		case first:
			t = recFirst
		case last:
			t = recLast
		}
		var h [logHeader]byte
		c := crc32.Update(crc32.Checksum([]byte{t}, castagnoli), castagnoli, data[:n])
		binary.LittleEndian.PutUint32(h[0:4], maskCRC(c))
		binary.LittleEndian.PutUint16(h[4:6], uint16(n))
		h[6] = t
		l.w.Write(h[:])
		l.w.Write(data[:n])
		l.off += logHeader + n
		data = data[n:]
		first = false
		if last {
			return
		}
	}
}

// readLog is the records of a LevelDB log file.
func readLog(b []byte) ([][]byte, error) {
	var out [][]byte
	var cur []byte
	in := false
	for off := 0; off < len(b); {
		blockLeft := logBlock - off%logBlock
		if blockLeft < logHeader {
			off += blockLeft
			continue
		}
		if off+logHeader > len(b) {
			break
		}
		h := b[off : off+logHeader]
		n := int(binary.LittleEndian.Uint16(h[4:6]))
		t := h[6]
		if t == 0 && n == 0 {
			// Zero padding to the end of a block.
			off += blockLeft
			continue
		}
		if off+logHeader+n > len(b) || logHeader+n > blockLeft {
			return nil, fmt.Errorf("a record at offset %d runs past its block", off)
		}
		data := b[off+logHeader : off+logHeader+n]
		c := crc32.Update(crc32.Checksum([]byte{t}, castagnoli), castagnoli, data)
		if maskCRC(c) != binary.LittleEndian.Uint32(h[0:4]) {
			return nil, fmt.Errorf("the record at offset %d fails its checksum", off)
		}
		switch t {
		case recFull:
			out = append(out, append([]byte(nil), data...))
		case recFirst:
			cur, in = append([]byte(nil), data...), true
		case recMiddle, recLast:
			if !in {
				return nil, fmt.Errorf("a record fragment at offset %d has no first fragment", off)
			}
			cur = append(cur, data...)
			if t == recLast {
				out, in = append(out, cur), false
			}
		default:
			return nil, fmt.Errorf("record type %d at offset %d", t, off)
		}
		off += logHeader + n
	}
	return out, nil
}

// connection state, kept beside Admin's.
type emulatorConn struct {
	connOnce sync.Once
	conn     *grpc.ClientConn
	connErr  error
}
