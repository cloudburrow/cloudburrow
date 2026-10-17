package bigtablefront

import (
	"context"
	"strings"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// UndeleteTable (#1511): DeleteTable through the front first copies the
// table (its column families and every cell, as a backup does, backups.go)
// and keeps the copy for the window Google documents, seven days;
// UndeleteTable within it writes the table back under its name, which
// must be free. A table the instance's deletion drops is not kept, as
// Google keeps none, and the copies are in memory, as the emulator's
// tables are.

const undeleteWindow = 7 * 24 * time.Hour

type deletedTable struct {
	data *tableCopy
	at   time.Time
}

// keepDeleted keeps the copy of the table name, deleted now.
func (c *copies) keepDeleted(name string, data *tableCopy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deleted == nil {
		c.deleted = map[string]*deletedTable{}
	}
	c.deleted[name] = &deletedTable{data: data, at: time.Now()}
}

// dropDeleted forgets the deleted tables of an instance.
func (c *copies) dropDeleted(instance string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for name := range c.deleted {
		if instanceOf(name) == instance {
			delete(c.deleted, name)
		}
	}
}

func (c *copies) undeleteTable(ctx context.Context, req *btapb.UndeleteTableRequest) (proto.Message, error) {
	name := req.GetName()
	if !tableNameRe.MatchString(name) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid name %q: want projects/{project}/instances/{instance}/tables/{table}", name)
	}
	c.mu.Lock()
	d, ok := c.deleted[name]
	if ok && time.Since(d.at) > undeleteWindow {
		delete(c.deleted, name)
		ok = false
	}
	c.mu.Unlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "Table %s was not deleted in the last %d days, so it cannot be undeleted", name, int(undeleteWindow/(24*time.Hour)))
	}
	t, err := c.write(ctx, instanceOf(name), name[strings.LastIndexByte(name, '/')+1:], d.data)
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return nil, status.Errorf(codes.FailedPrecondition, "Table %s exists: a deleted table is undeleted under its name, which must be free", name)
		}
		return nil, err
	}
	c.mu.Lock()
	delete(c.deleted, name)
	c.mu.Unlock()
	now := timestamppb.Now()
	return c.op(t.GetName(), &btapb.UndeleteTableMetadata{Name: t.GetName(), StartTime: now, EndTime: now}, t)
}
