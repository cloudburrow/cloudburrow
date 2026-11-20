package firestoreadmin

// Datastore mode databases (#1880). The Admin API records them; their
// entities are the Datastore emulator's, in the Datastore pod, which the
// Datastore call front serves by database ID once Access lets it
// (access.go). Deleting one has that front delete its entities, in every
// namespace, over the cluster (frontstate.DatastoreClearPath), and its
// exports, imports, bulk deletes, backups, restores and clones copy them
// through the same front (#1884): exports and imports in the Datastore
// Admin API's export format (frontstate.DatastoreExportPath and
// DatastoreImportPath), the others as google.datastore.v1 Entity messages
// (DatastoreDumpPath, DatastoreLoadPath and DatastoreDeletePath).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/frontstate"
)

// SetDatastore names the Datastore call front (host:port), which clears a
// deleted Datastore mode database's entities; "" when Datastore is not
// served, and a Datastore mode database then has no entities to clear.
func (a *Admin) SetDatastore(addr string) { a.datastore = addr }

// clearEntities has the Datastore front delete every entity of the
// database dbName.
func (a *Admin) clearEntities(ctx context.Context, dbName string) error {
	m := dbNameRe.FindStringSubmatch(dbName)
	if a.datastore == "" || m == nil {
		return nil
	}
	u := "http://" + a.datastore + frontstate.DatastoreClearPath + "?project=" + url.QueryEscape(m[1]) + "&database=" + url.QueryEscape(m[2])
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return status.Errorf(codes.Internal, "clearing %s: %v", dbName, err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return status.Errorf(codes.Unavailable, "the Datastore front did not clear the entities of %s: %v", dbName, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return status.Errorf(codes.Unavailable, "the Datastore front answered %s clearing the entities of %s: %s", resp.Status, dbName, strings.TrimSpace(string(b)))
	}
	return nil
}

func isDatastoreMode(db *adminpb.Database) bool {
	return db.GetType() == adminpb.Database_DATASTORE_MODE
}

// datastoreCopy is what the Datastore front's copy paths answer.
type datastoreCopy struct {
	Entities  int64  `json:"entities"`
	Bytes     int64  `json:"bytes"`
	OutputURI string `json:"outputUri,omitempty"`
}

// datastoreCopyRequest is the body of the Datastore front's export,
// import and delete paths.
type datastoreCopyRequest struct {
	Kinds           []string `json:"kinds,omitempty"`
	Namespaces      []string `json:"namespaces,omitempty"`
	OutputURIPrefix string   `json:"outputUriPrefix,omitempty"`
	Exact           bool     `json:"exact,omitempty"`
	InputURI        string   `json:"inputUri,omitempty"`
	// ReadTime reads the entities as they were then (#1967), RFC 3339.
	ReadTime string `json:"readTime,omitempty"`
}

// callDatastore posts body to the Datastore front's path for the database
// dbName, and answers the reply's body.
func (a *Admin) callDatastore(ctx context.Context, path, dbName string, body []byte) ([]byte, error) {
	m := dbNameRe.FindStringSubmatch(dbName)
	if m == nil {
		return nil, status.Errorf(codes.InvalidArgument, "%q is not a database name", dbName)
	}
	if a.datastore == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "the entities of the Datastore mode database %s are not reachable: Datastore is not served on this instance. Start the instance with datastore in --services", dbName)
	}
	u := "http://" + a.datastore + path + "?project=" + url.QueryEscape(m[1]) + "&database=" + url.QueryEscape(m[2])
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "copying %s: %v", dbName, err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "the Datastore front did not answer for %s: %v", dbName, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "the Datastore front's answer for %s: %v", dbName, err)
	}
	if resp.StatusCode == http.StatusConflict {
		var e struct {
			Code    codes.Code `json:"code"`
			Message string     `json:"message"`
		}
		if json.Unmarshal(b, &e) == nil {
			return nil, status.Error(e.Code, e.Message)
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, status.Errorf(codes.Unavailable, "the Datastore front answered %s for %s: %s", resp.Status, dbName, strings.TrimSpace(string(b)))
	}
	return b, nil
}

// copyDatastore calls one of the Datastore front's export, import and
// delete paths.
func (a *Admin) copyDatastore(ctx context.Context, path, dbName string, req datastoreCopyRequest) (datastoreCopy, error) {
	body, _ := json.Marshal(req)
	b, err := a.callDatastore(ctx, path, dbName, body)
	if err != nil {
		return datastoreCopy{}, err
	}
	var out datastoreCopy
	if err := json.Unmarshal(b, &out); err != nil {
		return datastoreCopy{}, status.Errorf(codes.Unavailable, "the Datastore front's answer for %s: %v", dbName, err)
	}
	return out, nil
}

// dumpEntities is every entity of the Datastore mode database dbName, each
// a google.datastore.v1 Entity message.
func (a *Admin) dumpEntities(ctx context.Context, dbName string, at *timestamppb.Timestamp) ([][]byte, error) {
	var body []byte
	if at != nil {
		body, _ = json.Marshal(datastoreCopyRequest{ReadTime: at.AsTime().UTC().Format(time.RFC3339Nano)})
	}
	b, err := a.callDatastore(ctx, frontstate.DatastoreDumpPath, dbName, body)
	if err != nil {
		return nil, err
	}
	parts, err := frontstate.Unpack(b)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "the entities of %s: %v", dbName, err)
	}
	return parts, nil
}

// loadEntities writes entities, read by dumpEntities, into the Datastore
// mode database dbName.
func (a *Admin) loadEntities(ctx context.Context, dbName string, entities [][]byte) error {
	_, err := a.callDatastore(ctx, frontstate.DatastoreLoadPath, dbName, frontstate.Pack(entities...))
	return err
}

func partsBytes(parts [][]byte) int64 {
	var n int64
	for _, p := range parts {
		n += int64(len(p))
	}
	return n
}
