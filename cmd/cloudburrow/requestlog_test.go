package main

import (
	"bytes"
	kmsapi "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"context"
	"net"
	"strings"
	"testing"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"log/slog"

	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	kmssvc "github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
)

func loggedServer(t *testing.T, level string, service string, register func(*grpc.Server)) (*bytes.Buffer, []option.ClientOption) {
	t.Helper()
	lv, err := grpctransport.ParseLevel(level)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	logger := slog.New(grpctransport.NewLineHandler(&buf, lv))
	g := grpc.NewServer(grpc.ChainUnaryInterceptor(grpctransport.LogInterceptor(logger, service)))
	register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	return &buf, []option.ClientOption{option.WithEndpoint(ln.Addr().String()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

func TestRequestLogLineForANotFoundGetQueue(t *testing.T) {
	buf, opts := loggedServer(t, "info", "tasks", func(g *grpc.Server) { tasks.NewGRPCServer(tasks.NewStore(store.NewMemory())).Register(g) })
	c, err := cloudtasks.NewClient(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	name := "projects/demo-project/locations/us-central1/queues/absent"
	_, err = c.GetQueue(context.Background(), &cloudtaskspb.GetQueueRequest{Name: name})
	want := "INFO  tasks.GetQueue => NOT_FOUND (" + status.Convert(err).Message() + ")\n"
	if buf.String() != want {
		t.Errorf("log = %q\nwant  %q", buf.String(), want)
	}

	// A successful call logs nothing at info.
	buf.Reset()
	if _, err := c.ListQueues(context.Background(), &cloudtaskspb.ListQueuesRequest{Parent: "projects/demo-project/locations/us-central1"}).Next(); err != nil && !strings.Contains(err.Error(), "no more items") {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("a successful call logged at info: %q", buf.String())
	}
}

func TestRequestLogAtDebugLogsEveryCall(t *testing.T) {
	buf, opts := loggedServer(t, "debug", "tasks", func(g *grpc.Server) { tasks.NewGRPCServer(tasks.NewStore(store.NewMemory())).Register(g) })
	c, err := cloudtasks.NewClient(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.ListQueues(context.Background(), &cloudtaskspb.ListQueuesRequest{Parent: "projects/demo-project/locations/us-central1"}).Next()
	if got := buf.String(); got != "DEBUG tasks.ListQueues => OK\n" {
		t.Errorf("debug log = %q", got)
	}
}

// At trace the metadata is logged, but never a credential header's value,
// and never a body: a secret payload cannot reach the log.
func TestRequestLogAtTraceRedactsCredentialsAndNeverLogsBodies(t *testing.T) {
	st := secrets.NewStore(store.NewMemory())
	buf, opts := loggedServer(t, "trace", "secretmanager", func(g *grpc.Server) { secrets.NewGRPCServer(st).Register(g) })
	c, err := secretmanager.NewClient(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		"authorization", "Bearer super-secret-token", "metadata-flavor", "Google", "x-custom", "visible")
	sec, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/demo-project", SecretId: "s",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: sec.Name,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("the-payload-value")}}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, secret := range []string{"super-secret-token", "the-payload-value"} {
		if strings.Contains(out, secret) {
			t.Errorf("the log holds %q:\n%s", secret, out)
		}
	}
	for _, want := range []string{"TRACE secretmanager.AddSecretVersion metadata:", "authorization=[REDACTED]", "metadata-flavor=[REDACTED]", "x-custom=visible"} {
		if !strings.Contains(out, want) {
			t.Errorf("the trace log lacks %q:\n%s", want, out)
		}
	}
}

// Cloud KMS is logged like the other in-process services (#392): a failing
// call at info, a succeeding one only at debug.
func TestKMSRequestLogLines(t *testing.T) {
	for level, wantSuccessLogged := range map[string]bool{"info": false, "debug": true} {
		buf, opts := loggedServer(t, level, "kms", func(g *grpc.Server) { kmssvc.NewServer(store.NewMemory()).Register(g) })
		c, err := kmsapi.NewKeyManagementClient(context.Background(), opts...)
		if err != nil {
			t.Fatal(err)
		}
		name := "projects/demo-project/locations/global/keyRings/absent"
		_, err = c.GetKeyRing(context.Background(), &kmspb.GetKeyRingRequest{Name: name})
		want := "kms.GetKeyRing => NOT_FOUND (" + status.Convert(err).Message() + ")"
		if !strings.Contains(buf.String(), "INFO  "+want) {
			t.Errorf("%s: log = %q, want an INFO line %q", level, buf.String(), want)
		}
		buf.Reset()
		if _, err := c.CreateKeyRing(context.Background(), &kmspb.CreateKeyRingRequest{Parent: "projects/demo-project/locations/global", KeyRingId: "r"}); err != nil {
			t.Fatal(err)
		}
		if logged := buf.Len() > 0; logged != wantSuccessLogged {
			t.Errorf("%s: a successful call logged = %v (%q), want %v", level, logged, buf.String(), wantSuccessLogged)
		}
		_ = c.Close()
	}
}
