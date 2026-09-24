package main

import (
	"context"
	"fmt"
	"net"
	"sort"
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
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/scheduler"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
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
	rmpb.RegisterProjectsServer(srv, resourcemanager.NewProjectsServer(resourcemanager.New(store.NewMemory())))
	schedStore := scheduler.NewStore(store.NewMemory())
	scheduler.NewGRPCServer(schedStore, scheduler.NewRunner(schedStore, nil, nil, nil, time.Second), nil).Register(srv)
	// No Knative: every Cloud Run handler validates its request before
	// reaching the cluster, and an empty request never gets that far.
	runSrv := runadapter.NewServer(nil, "coverage", time.Second)
	runSrv.Register(srv)
	runSrv.Revisions().Register(srv)
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
