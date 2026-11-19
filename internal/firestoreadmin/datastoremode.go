package firestoreadmin

// Datastore mode databases (#1880). The Admin API records them; their
// entities are the Datastore emulator's, in the Datastore pod, which the
// Datastore call front serves by database ID once Access lets it
// (access.go). Deleting one has that front delete its entities, in every
// namespace, over the cluster (frontstate.DatastoreClearPath).

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"

	"github.com/cloudburrow/cloudburrow/internal/frontstate"
)

// documentsOnly refuses what copies a database's documents (what) for a
// Datastore mode database, whose entities are the Datastore emulator's:
// its exports, imports, backups, restores and clones are not served yet.
func documentsOnly(db *adminpb.Database, what string) error {
	if db.GetType() != adminpb.Database_DATASTORE_MODE {
		return nil
	}
	return status.Errorf(codes.Unimplemented, "%s of a Datastore mode database (%s) are not served here: its entities are the Datastore emulator's, and only a Native mode database's documents are copied", what, db.GetName())
}

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
