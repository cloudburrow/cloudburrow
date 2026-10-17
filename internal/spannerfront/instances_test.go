package spannerfront_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/callfront"
	"github.com/cloudburrow/cloudburrow/internal/calllog"
	"github.com/cloudburrow/cloudburrow/internal/spannerfront"
)

// fakeEmulator keeps instances, as the emulator does, and refuses updates.
type fakeEmulator struct {
	instancepb.UnimplementedInstanceAdminServer
	insts map[string]*instancepb.Instance
}

func (f *fakeEmulator) GetInstance(_ context.Context, r *instancepb.GetInstanceRequest) (*instancepb.Instance, error) {
	if i, ok := f.insts[r.Name]; ok {
		return i, nil
	}
	return nil, status.Error(codes.NotFound, "Instance not found")
}

func (f *fakeEmulator) ListInstances(context.Context, *instancepb.ListInstancesRequest) (*instancepb.ListInstancesResponse, error) {
	resp := &instancepb.ListInstancesResponse{}
	for _, i := range f.insts {
		resp.Instances = append(resp.Instances, i)
	}
	return resp, nil
}

func (f *fakeEmulator) DeleteInstance(_ context.Context, r *instancepb.DeleteInstanceRequest) (*emptypb.Empty, error) {
	delete(f.insts, r.Name)
	return &emptypb.Empty{}, nil
}

var lastConn *grpc.ClientConn

// fakeDatabases keeps one database, d.
type fakeDatabases struct {
	databasepb.UnimplementedDatabaseAdminServer
}

func (fakeDatabases) GetDatabase(_ context.Context, r *databasepb.GetDatabaseRequest) (*databasepb.Database, error) {
	if r.Name == "projects/p/instances/i/databases/d" {
		return &databasepb.Database{Name: r.Name}, nil
	}
	return nil, status.Error(codes.NotFound, "Database not found")
}

func (fakeDatabases) GetDatabaseDdl(_ context.Context, r *databasepb.GetDatabaseDdlRequest) (*databasepb.GetDatabaseDdlResponse, error) {
	if r.Database == "projects/p/instances/i/databases/d" {
		return &databasepb.GetDatabaseDdlResponse{Statements: []string{
			"CREATE TABLE Items (\n  Id INT64 NOT NULL,\n  Name STRING(MAX),\n) PRIMARY KEY(Id)",
			"CREATE INDEX ItemsByName ON Items(Name)",
		}}, nil
	}
	return nil, status.Error(codes.NotFound, "Database not found")
}

func serve(t *testing.T) (string, instancepb.InstanceAdminClient) {
	t.Helper()
	emu := &fakeEmulator{insts: map[string]*instancepb.Instance{
		"projects/p/instances/i": {Name: "projects/p/instances/i", DisplayName: "first", NodeCount: 1, ProcessingUnits: 1000}}}
	el, _ := net.Listen("tcp", "127.0.0.1:0")
	gs := grpc.NewServer()
	instancepb.RegisterInstanceAdminServer(gs, emu)
	databasepb.RegisterDatabaseAdminServer(gs, fakeDatabases{})
	go func() { _ = gs.Serve(el) }()
	t.Cleanup(gs.Stop)
	si, err := spannerfront.New(el.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fl, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := callfront.Server(callfront.HandlerWith(el.Addr().String(), &calllog.Ring{}, t.Logf, callfront.Intercepts{si, spannerfront.NewRefusals()}), t.Logf)
	go func() { _ = srv.Serve(fl) }()
	t.Cleanup(func() { _ = srv.Close() })
	conn, err := grpc.NewClient(fl.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	lastConn = conn
	return fl.Addr().String(), instancepb.NewInstanceAdminClient(conn)
}

func TestUpdateInstanceOverGRPC(t *testing.T) {
	_, c := serve(t)
	ctx := context.Background()
	name := "projects/p/instances/i"
	op, err := c.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{
		Instance:  &instancepb.Instance{Name: name, DisplayName: "renamed", ProcessingUnits: 500, Labels: map[string]string{"a": "b"}},
		FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "processing_units", "labels"}}})
	if err != nil || !op.Done {
		t.Fatalf("UpdateInstance = %v, %v", op, err)
	}
	got, err := c.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: name})
	if err != nil || got.DisplayName != "renamed" || got.ProcessingUnits != 500 || got.NodeCount != 0 || got.Labels["a"] != "b" {
		t.Fatalf("GetInstance = %v, %v", got, err)
	}
	list, err := c.ListInstances(ctx, &instancepb.ListInstancesRequest{Parent: "projects/p"})
	if err != nil || len(list.Instances) != 1 || list.Instances[0].DisplayName != "renamed" {
		t.Fatalf("ListInstances = %v, %v", list, err)
	}
	for mask, want := range map[string]codes.Code{"autoscaling_config": codes.Unimplemented, "config": codes.Unimplemented, "state": codes.InvalidArgument} {
		_, err := c.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{Instance: &instancepb.Instance{Name: name, Config: "projects/p/instanceConfigs/other",
			AutoscalingConfig: &instancepb.AutoscalingConfig{}},
			FieldMask: &fieldmaskpb.FieldMask{Paths: []string{mask}}})
		if status.Code(err) != want {
			t.Errorf("mask %s: %v, want %v", mask, err, want)
		}
	}
	if _, err := c.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{Instance: &instancepb.Instance{Name: name, ProcessingUnits: 1500},
		FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"processing_units"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("1500 processing units: %v, want InvalidArgument", err)
	}
	if _, err := c.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{Instance: &instancepb.Instance{Name: "projects/p/instances/nope", NodeCount: 2},
		FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"node_count"}}}); status.Code(err) != codes.NotFound {
		t.Errorf("a missing instance: %v, want NotFound", err)
	}
	if _, err := c.DeleteInstance(ctx, &instancepb.DeleteInstanceRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateInstanceOverREST(t *testing.T) {
	addr, _ := serve(t)
	do := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	// As gcloud sends it: autoscaling cleared beside the node count.
	code, op := do("PATCH", "/v1/projects/p/instances/i", `{"instance":{"nodeCount":3,"displayName":"by rest"},"fieldMask":"nodeCount,displayName,autoscalingConfig"}`)
	if code != 200 || op["done"] != true {
		t.Fatalf("patch = %d %v", code, op)
	}
	if code, got := do("GET", "/v1/"+op["name"].(string), ""); code != 200 || got["done"] != true {
		t.Errorf("the operation = %d %v, want it done", code, got)
	}
	code, got := do("GET", "/v1/projects/p/instances/i", "")
	if code != 200 || got["nodeCount"] != float64(3) || got["processingUnits"] != float64(3000) || got["displayName"] != "by rest" {
		t.Fatalf("get = %d %v", code, got)
	}
	// As the Terraform provider sends it.
	if code, got = do("PATCH", "/v1/projects/p/instances/i/databases/d?updateMask=enableDropProtection", `{"enableDropProtection":true}`); code != 200 || got["done"] != true {
		t.Errorf("UpdateDatabase over REST = %d %v", code, got)
	}
	if code, got = do("PATCH", "/v1/projects/p/instances/i", `{"instance":{"autoscalingConfig":{}},"fieldMask":"autoscalingConfig"}`); code != 501 {
		t.Errorf("autoscaling = %d %v, want 501", code, got)
	}
}

func TestRefusalsNameWhy(t *testing.T) {
	serve(t)
	ctx := context.Background()
	_, err := databasepb.NewDatabaseAdminClient(lastConn).InternalUpdateGraphOperation(ctx, &databasepb.InternalUpdateGraphOperationRequest{Database: "projects/p/instances/i/databases/d"})
	if status.Code(err) != codes.Unimplemented || !strings.Contains(status.Convert(err).Message(), "internal") {
		t.Errorf("InternalUpdateGraphOperation: %v, want UNIMPLEMENTED naming why", err)
	}
}

// TestAddSplitPointsIsAStoredNoOp (#1523): split points on a table and an
// index of the database are accepted, over gRPC and REST, and refused as
// Google refuses them: an unknown table or index, an index named with
// another table, a split point without keys, an expiry beyond 30 days, an
// unknown database.
func TestAddSplitPointsIsAStoredNoOp(t *testing.T) {
	addr, _ := serve(t)
	ctx := context.Background()
	c := databasepb.NewDatabaseAdminClient(lastConn)
	db := "projects/p/instances/i/databases/d"
	key := func(v string) []*databasepb.SplitPoints_Key {
		return []*databasepb.SplitPoints_Key{{KeyParts: &structpb.ListValue{Values: []*structpb.Value{structpb.NewStringValue(v)}}}}
	}
	if _, err := c.AddSplitPoints(ctx, &databasepb.AddSplitPointsRequest{Database: db, SplitPoints: []*databasepb.SplitPoints{
		{Table: "Items", Keys: key("100")},
		{Index: "ItemsByName", Keys: key("m"), ExpireTime: timestamppb.New(time.Now().Add(24 * time.Hour))},
	}}); err != nil {
		t.Fatalf("AddSplitPoints: %v", err)
	}
	for _, tc := range []struct {
		what string
		sp   *databasepb.SplitPoints
		db   string
		code codes.Code
	}{
		{"an unknown table", &databasepb.SplitPoints{Table: "Nope", Keys: key("1")}, db, codes.NotFound},
		{"an unknown index", &databasepb.SplitPoints{Index: "Nope", Keys: key("1")}, db, codes.NotFound},
		{"an index named with another table", &databasepb.SplitPoints{Table: "Other", Index: "ItemsByName", Keys: key("1")}, db, codes.InvalidArgument},
		{"no keys", &databasepb.SplitPoints{Table: "Items"}, db, codes.InvalidArgument},
		{"an expiry 31 days ahead", &databasepb.SplitPoints{Table: "Items", Keys: key("1"), ExpireTime: timestamppb.New(time.Now().Add(31 * 24 * time.Hour))}, db, codes.InvalidArgument},
		{"an unknown database", &databasepb.SplitPoints{Table: "Items", Keys: key("1")}, "projects/p/instances/i/databases/gone", codes.NotFound},
	} {
		_, err := c.AddSplitPoints(ctx, &databasepb.AddSplitPointsRequest{Database: tc.db, SplitPoints: []*databasepb.SplitPoints{tc.sp}})
		if status.Code(err) != tc.code {
			t.Errorf("AddSplitPoints with %s: %v, want %s", tc.what, err, tc.code)
		}
	}
	req, _ := http.NewRequest("POST", "http://"+addr+"/v1/"+db+":addSplitPoints",
		strings.NewReader(`{"splitPoints":[{"table":"Items","keys":[{"keyParts":["200"]}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("REST addSplitPoints = %d, want 200", resp.StatusCode)
	}
}
