package spannerfront

import (
	"context"
	"io"
	"strings"

	"cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

// A customer-managed database's data calls (#1950, cmek.go): while any
// database has a customer-managed key, the front claims the Spanner data
// calls that name a database or one of its sessions, refuses them with
// Google's FAILED_PRECONDITION while the database's key version cannot
// unwrap its data key, and passes them to the emulator otherwise. With no
// such database nothing is claimed, and every data call goes to the
// emulator as before.

const dataService = "/google.spanner.v1.Spanner/"

// keyedDataMethods are the data calls the front checks.
var keyedDataMethods = map[string]bool{
	"CreateSession": true, "BatchCreateSessions": true, "GetSession": true, "DeleteSession": true,
	"ExecuteSql": true, "ExecuteStreamingSql": true, "ExecuteBatchDml": true, "Read": true, "StreamingRead": true,
	"BeginTransaction": true, "Commit": true, "Rollback": true, "PartitionQuery": true, "PartitionRead": true,
	"BatchWrite": true,
}

// databaseOfSession is the database a session's name is under.
func databaseOfSession(session string) string {
	if i := strings.Index(session, "/sessions/"); i >= 0 {
		return session[:i]
	}
	return session
}

func (d cacheData) check(ctx context.Context, name string) error {
	return d.s.databaseAccessible(ctx, databaseOfSession(name))
}

func (d cacheData) CreateSession(ctx context.Context, req *spannerpb.CreateSessionRequest) (*spannerpb.Session, error) {
	if err := d.check(ctx, req.GetDatabase()); err != nil {
		return nil, err
	}
	return d.s.data.CreateSession(ctx, req)
}

func (d cacheData) BatchCreateSessions(ctx context.Context, req *spannerpb.BatchCreateSessionsRequest) (*spannerpb.BatchCreateSessionsResponse, error) {
	if err := d.check(ctx, req.GetDatabase()); err != nil {
		return nil, err
	}
	return d.s.data.BatchCreateSessions(ctx, req)
}

func (d cacheData) GetSession(ctx context.Context, req *spannerpb.GetSessionRequest) (*spannerpb.Session, error) {
	if err := d.check(ctx, req.GetName()); err != nil {
		return nil, err
	}
	return d.s.data.GetSession(ctx, req)
}

// DeleteSession is never refused: a client closing its sessions is not
// reading the database.
func (d cacheData) DeleteSession(ctx context.Context, req *spannerpb.DeleteSessionRequest) (*emptypb.Empty, error) {
	return d.s.data.DeleteSession(ctx, req)
}

func (d cacheData) ExecuteSql(ctx context.Context, req *spannerpb.ExecuteSqlRequest) (*spannerpb.ResultSet, error) {
	if err := d.check(ctx, req.GetSession()); err != nil {
		return nil, err
	}
	return d.s.data.ExecuteSql(ctx, req)
}

func (d cacheData) ExecuteBatchDml(ctx context.Context, req *spannerpb.ExecuteBatchDmlRequest) (*spannerpb.ExecuteBatchDmlResponse, error) {
	if err := d.check(ctx, req.GetSession()); err != nil {
		return nil, err
	}
	return d.s.data.ExecuteBatchDml(ctx, req)
}

func (d cacheData) Read(ctx context.Context, req *spannerpb.ReadRequest) (*spannerpb.ResultSet, error) {
	if err := d.check(ctx, req.GetSession()); err != nil {
		return nil, err
	}
	return d.s.data.Read(ctx, req)
}

func (d cacheData) BeginTransaction(ctx context.Context, req *spannerpb.BeginTransactionRequest) (*spannerpb.Transaction, error) {
	if err := d.check(ctx, req.GetSession()); err != nil {
		return nil, err
	}
	return d.s.data.BeginTransaction(ctx, req)
}

func (d cacheData) Commit(ctx context.Context, req *spannerpb.CommitRequest) (*spannerpb.CommitResponse, error) {
	if err := d.check(ctx, req.GetSession()); err != nil {
		return nil, err
	}
	return d.s.data.Commit(ctx, req)
}

func (d cacheData) Rollback(ctx context.Context, req *spannerpb.RollbackRequest) (*emptypb.Empty, error) {
	if err := d.check(ctx, req.GetSession()); err != nil {
		return nil, err
	}
	return d.s.data.Rollback(ctx, req)
}

func (d cacheData) PartitionQuery(ctx context.Context, req *spannerpb.PartitionQueryRequest) (*spannerpb.PartitionResponse, error) {
	if err := d.check(ctx, req.GetSession()); err != nil {
		return nil, err
	}
	return d.s.data.PartitionQuery(ctx, req)
}

func (d cacheData) PartitionRead(ctx context.Context, req *spannerpb.PartitionReadRequest) (*spannerpb.PartitionResponse, error) {
	if err := d.check(ctx, req.GetSession()); err != nil {
		return nil, err
	}
	return d.s.data.PartitionRead(ctx, req)
}

// relay sends every message of a server stream on to out.
func relay[T any](in interface{ Recv() (*T, error) }, out interface{ Send(*T) error }) error {
	for {
		m, err := in.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := out.Send(m); err != nil {
			return err
		}
	}
}

func (d cacheData) ExecuteStreamingSql(req *spannerpb.ExecuteSqlRequest, stream spannerpb.Spanner_ExecuteStreamingSqlServer) error {
	if err := d.check(stream.Context(), req.GetSession()); err != nil {
		return err
	}
	in, err := d.s.data.ExecuteStreamingSql(stream.Context(), req)
	if err != nil {
		return err
	}
	return relay[spannerpb.PartialResultSet](in, stream)
}

func (d cacheData) StreamingRead(req *spannerpb.ReadRequest, stream spannerpb.Spanner_StreamingReadServer) error {
	if err := d.check(stream.Context(), req.GetSession()); err != nil {
		return err
	}
	in, err := d.s.data.StreamingRead(stream.Context(), req)
	if err != nil {
		return err
	}
	return relay[spannerpb.PartialResultSet](in, stream)
}

func (d cacheData) BatchWrite(req *spannerpb.BatchWriteRequest, stream spannerpb.Spanner_BatchWriteServer) error {
	if err := d.check(stream.Context(), req.GetSession()); err != nil {
		return err
	}
	in, err := d.s.data.BatchWrite(stream.Context(), req)
	if err != nil {
		return err
	}
	return relay[spannerpb.BatchWriteResponse](in, stream)
}
