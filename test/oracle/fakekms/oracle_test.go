package oracle

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	cbkms "github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/test/oracle/fakekms/internal/fakekms"
)

// The oracle (#419) runs one request sequence against fakekms and against
// CloudBurrow and compares, for every call, the status code and the response
// fields after server-set times, IDs and page tokens are normalised. It ports
// the pattern of kms-integrations' contract tests (fakekms/contract/
// contract_test.go:43-103, which run one suite against either server) to a
// side-by-side comparison.
//
// Agreement with fakekms is NOT an observation of Google, and never removes
// an UNVERIFIED annotation. The oracle is never pointed at live Google.

const loc = "projects/oracle-project/locations/global"

// clients returns an official gRPC client for each server, over loopback.
func clients(t *testing.T) (fake, cb *kms.KeyManagementClient) {
	t.Helper()
	ctx := context.Background()
	dial := func(addr string) *kms.KeyManagementClient {
		c, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	fs, err := fakekms.NewServer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fs.Close)

	g := grpc.NewServer()
	cbkms.NewServer(store.NewMemory()).Register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	return dial(fs.Addr.String()), dial(ln.Addr().String())
}

// step is one call, made the same way on each server.
type step struct {
	name string
	// call returns a message, or a list's items in order.
	call func(ctx context.Context, c *kms.KeyManagementClient) (any, error)
}

// outcome is a call's normalised result.
type outcome struct {
	Code string
	Body map[string]any
}

func run(ctx context.Context, c *kms.KeyManagementClient, s step) outcome {
	at := time.Now()
	m, err := s.call(ctx, c)
	if err != nil {
		return outcome{Code: status.Code(err).String()}
	}
	o := outcome{Code: "OK", Body: map[string]any{}}
	asMap := func(pm proto.Message) any {
		var v any
		b, _ := protojson.Marshal(pm)
		_ = json.Unmarshal(b, &v)
		return v
	}
	switch x := m.(type) {
	case proto.Message:
		o.Body["response"] = asMap(x)
	case []proto.Message:
		items := []any{}
		for _, e := range x {
			items = append(items, asMap(e))
		}
		o.Body["items"] = items
	}
	normalise(o.Body, at)
	return o
}

// normalise replaces what a server sets for itself, which no two servers can
// agree on, with whether it is set: every Timestamp (a key ending in "Time")
// and page tokens. destroyTime is the exception: it is scheduled from the
// key's destroy_scheduled_duration, so it becomes its distance from the call,
// to the hour, and a different schedule shows.
func normalise(v any, at time.Time) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			switch {
			case k == "destroyTime":
				if ts, ok := e.(string); ok {
					if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
						x[k] = fmt.Sprintf("now+%dh", int(t.Sub(at).Round(time.Hour)/time.Hour))
						continue
					}
				}
				x[k] = "<set>"
			case strings.HasSuffix(k, "Time") || k == "nextPageToken":
				x[k] = "<set>"
			default:
				normalise(e, at)
			}
		}
	case []any:
		for _, e := range x {
			normalise(e, at)
		}
	}
}

// diff lists how two outcomes differ, as JSON paths, "code" included.
func diff(fake, cb outcome) map[string]string {
	out := map[string]string{}
	if fake.Code != cb.Code {
		out["code"] = fmt.Sprintf("fakekms %s, CloudBurrow %s", fake.Code, cb.Code)
	}
	var walk func(path string, a, b any)
	walk = func(path string, a, b any) {
		am, aok := a.(map[string]any)
		bm, bok := b.(map[string]any)
		if aok && bok {
			keys := map[string]bool{}
			for k := range am {
				keys[k] = true
			}
			for k := range bm {
				keys[k] = true
			}
			for k := range keys {
				walk(path+"."+k, am[k], bm[k])
			}
			return
		}
		al, alok := a.([]any)
		bl, blok := b.([]any)
		if alok && blok && len(al) == len(bl) {
			for i := range al {
				walk(fmt.Sprintf("%s[%d]", path, i), al[i], bl[i])
			}
			return
		}
		if !reflect.DeepEqual(a, b) {
			ja, _ := json.Marshal(a)
			jb, _ := json.Marshal(b)
			out[path] = fmt.Sprintf("fakekms %s, CloudBurrow %s", ja, jb)
		}
	}
	if fake.Code == "OK" && cb.Code == "OK" {
		walk("", fake.Body, cb.Body)
	}
	return out
}

// compare runs every step on both servers, in order, and fails on each
// difference that divergences does not list.
func compare(t *testing.T, steps []step) {
	t.Helper()
	fake, cb := clients(t)
	ctx := context.Background()
	for _, s := range steps {
		for path, d := range diff(run(ctx, fake, s), run(ctx, cb, s)) {
			if reason, ok := divergences[s.name+" "+path]; ok {
				t.Logf("known divergence: %s %s: %s (%s)", s.name, path, d, reason)
				continue
			}
			t.Errorf("%s %s: %s", s.name, path, d)
		}
	}
}

// drain collects a list's items in order.
func drain[T proto.Message](next func() (T, error)) (any, error) {
	var out []proto.Message
	for {
		m, err := next()
		if err == iterator.Done {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
}
