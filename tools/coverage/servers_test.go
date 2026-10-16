package main

import (
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"testing"
	"time"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/scheduler"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestTheRegistryMatchesTheServers calls every RPC of the services
// CloudBurrow implements, in-process, with an empty request, and holds the
// registry to what the servers do: an unimplemented or unserved method must
// return codes.Unimplemented, and an implemented one must not.
//
// In-process, over memory stores, because probing a live instance would run
// mutating methods against state someone cares about (#288). An empty
// request is enough to tell the two apart: an implemented handler rejects it
// as invalid or not found; only an unimplemented one answers UNIMPLEMENTED.
func TestTheRegistryMatchesTheServers(t *testing.T) {
	reg, err := loadRegistry("../..")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	tasks.NewGRPCServer(tasks.NewStore(store.NewMemory())).Register(srv)
	secrets.NewGRPCServer(secrets.NewStore(store.NewMemory())).Register(srv)
	kms.NewServer(store.NewMemory()).Register(srv)
	rmReg := resourcemanager.New(store.NewMemory())
	rmpb.RegisterProjectsServer(srv, resourcemanager.NewProjectsServer(rmReg))
	rmpb.RegisterFoldersServer(srv, resourcemanager.NewFoldersServer(rmReg))
	rmpb.RegisterOrganizationsServer(srv, resourcemanager.OrganizationsServer{})
	rmpb.RegisterTagKeysServer(srv, resourcemanager.NewTagKeysServer(rmReg))
	rmpb.RegisterTagValuesServer(srv, resourcemanager.NewTagValuesServer(rmReg))
	rmpb.RegisterTagBindingsServer(srv, resourcemanager.NewTagBindingsServer(rmReg))
	rmpb.RegisterTagHoldsServer(srv, resourcemanager.NewTagHoldsServer(rmReg))
	schedStore := scheduler.NewStore(store.NewMemory())
	scheduler.NewGRPCServer(schedStore, scheduler.NewRunner(schedStore, nil, nil, nil, time.Second), nil).Register(srv)
	// The Locations mixin is registered once, by KMS above (#1291): Cloud
	// Scheduler serves it too, on its own port, and the registry key is global.
	logging.NewServer(logging.NewStore(10)).Register(srv)
	redis := redisadmin.NewServer(redisadmin.Instance{Project: "coverage"}, nil)
	redis.EnableCreate(context.Background(), nil, nil, nil, nil)
	redis.Register(srv)
	vertexai.NewServer(nil).RegisterGRPC(srv)
	adminpb.RegisterFirestoreAdminServer(srv, firestoreadmin.New(""))
	logging.NewAdmin(store.NewMemory()).Register(srv)
	// No Knative: every Cloud Run handler validates its request before
	// reaching the cluster, and an empty request never gets that far.
	runSrv := runadapter.NewServer(nil, "coverage", time.Second)
	runSrv.Register(srv)
	runSrv.Revisions().Register(srv)
	runSrv.Jobs().Register(srv)
	runSrv.WorkerPools().Register(srv)
	runSrv.Instances().Register(srv)
	runSrv.Executions().Register(srv)
	runSrv.Tasks().Register(srv)
	runSrv.Builds(nil, nil).Register(srv)
	runSrv.ArtifactRegistry().Register(srv)
	runSrv.CloudBuild().Register(srv)
	// FunctionService alone: its IAM and Locations mixins are KMS's above.
	functionspb.RegisterFunctionServiceServer(srv, runSrv.Functions(runSrv.Builds(nil, nil), func() string { return "" }))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Stop()
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	methods := make([]string, 0, len(reg))
	for m := range reg {
		// Cloud Storage's registry is the server's own method table, and
		// TestStorageEveryUnbuiltMethodIsNotImplemented checks its 501s
		// over HTTP (internal/service/storage).
		if strings.HasPrefix(m, "storage.") {
			continue
		}
		// The Cloud SQL Admin API's registry is its server's method table,
		// and TestEveryUnservedMethodIsNotImplemented checks its 501s over
		// HTTP (internal/service/sqladmin).
		if strings.HasPrefix(m, "sql.") || strings.HasPrefix(m, "serviceusage.") {
			continue
		}
		// The Cloud Storage gRPC API's proto is registered under
		// CloudBurrow's own name, so TestGRPCRegistryMatchesTheServer checks
		// it in its own package (internal/service/storage).
		if strings.HasPrefix(m, "google.storage.v2.") {
			continue
		}
		methods = append(methods, m)
	}
	sort.Strings(methods)
	for _, full := range methods {
		want := reg[full]
		svcName, name, _ := cut(full)
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(svcName))
		if err != nil {
			t.Errorf("%s: %v", full, err)
			continue
		}
		md := d.(protoreflect.ServiceDescriptor).Methods().ByName(protoreflect.Name(name))
		if md == nil {
			t.Errorf("%s: no such method", full)
			continue
		}
		code := invoke(t, conn, full, md)
		unimplemented := code == codes.Unimplemented
		switch {
		case (want == regUnimplemented || want == regNotServed) && !unimplemented:
			t.Errorf("%s is %s in the registry, but the server returned %v", full, want, code)
		case want == regImplemented && unimplemented:
			t.Errorf("%s is implemented in the registry, but the server returned UNIMPLEMENTED", full)
		}
	}
}

func cut(full string) (service, method string, ok bool) {
	for i := len(full) - 1; i >= 0; i-- {
		if full[i] == '/' {
			return full[:i], full[i+1:], true
		}
	}
	return full, "", false
}

func invoke(t *testing.T, conn *grpc.ClientConn, full string, md protoreflect.MethodDescriptor) (code codes.Code) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s panicked on an empty request: %v", full, r)
			code = codes.Internal
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := fmt.Sprintf("/%s", full)
	if md.IsStreamingClient() || md.IsStreamingServer() {
		stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: md.IsStreamingServer(), ClientStreams: md.IsStreamingClient()}, path)
		if err != nil {
			return status.Code(err)
		}
		_ = stream.SendMsg(dynamicpb.NewMessage(md.Input()))
		_ = stream.CloseSend()
		return status.Code(stream.RecvMsg(dynamicpb.NewMessage(md.Output())))
	}
	return status.Code(conn.Invoke(ctx, path, dynamicpb.NewMessage(md.Input()), dynamicpb.NewMessage(md.Output())))
}
