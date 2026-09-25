package kms

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	kmsapi "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
)

// echoingKubectl is a fake kubectl that keeps Secrets in memory and fails
// every apply of a key version, echoing the whole manifest it was sent on
// stdin into its error: the worst a real kubectl could do (#390).
type echoingKubectl struct {
	mu       sync.Mutex
	secrets  map[string]string // secret name -> data.value
	material [][]byte          // key material it was sent
}

func (f *echoingKubectl) Run(_ context.Context, stdin string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "apply"):
		var m struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdin), &m); err != nil {
			return "", err
		}
		value, _ := base64.StdEncoding.DecodeString(m.Data["value"])
		var rec struct{ Material []byte }
		if json.Unmarshal(value, &rec) == nil && len(rec.Material) > 0 {
			f.material = append(f.material, rec.Material)
			return "", fmt.Errorf("kubectl: exit status 1: error when applying %q: %s", "STDIN", stdin)
		}
		f.secrets[m.Metadata.Name] = m.Data["value"]
		return "", nil
	case strings.Contains(joined, "get secret "):
		name := args[len(args)-3]
		if v, ok := f.secrets[name]; ok {
			return v, nil
		}
		return "", errors.New(`kubectl: exit status 1: Error from server (NotFound): secrets "` + name + `" not found`)
	}
	return "", nil
}

func (f *echoingKubectl) forms() []string {
	var out []string
	for _, m := range f.material {
		out = append(out, string(m), base64.StdEncoding.EncodeToString(m), base64.RawStdEncoding.EncodeToString(m),
			base64.URLEncoding.EncodeToString(m), hex.EncodeToString(m))
	}
	return out
}

// Key material never reaches the Put error, the status a client gets or the
// request log, even when kubectl echoes the manifest it was sent.
func TestKeyMaterialNeverReachesAKubectlError(t *testing.T) {
	fake := &echoingKubectl{secrets: map[string]string{}}
	var logged bytes.Buffer
	logger := slog.New(grpctransport.NewLineHandler(&logged, slog.LevelDebug))
	g := grpc.NewServer(grpc.UnaryInterceptor(grpctransport.LogInterceptor(logger, "kms")))
	NewServer(NewKubeStore(fake, "cloudburrow", "i")).Register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	ctx := context.Background()
	c, err := kmsapi.NewKeyManagementClient(ctx, option.WithEndpoint(ln.Addr().String()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "r"})
	if err != nil {
		t.Fatal(err)
	}
	_, cerr := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if cerr == nil {
		t.Fatal("CreateCryptoKey succeeded although the fake refused the version")
	}

	// And the store's own error, before it becomes a status.
	perr := NewKubeStore(fake, "cloudburrow", "i").Put("version-key", []byte(`{"Material":"c2VjcmV0LWtleS1tYXRlcmlhbC1mb3ItdGVzdC0xMjM0NTY3OA=="}`))
	if perr == nil || !strings.Contains(perr.Error(), redacted) {
		t.Fatalf("Put error = %v, want a redacted error", perr)
	}
	if len(fake.material) < 2 {
		t.Fatalf("the fake saw %d key versions, want the created key's and the direct Put's", len(fake.material))
	}
	for where, text := range map[string]string{"Put error": perr.Error(), "client status": cerr.Error(), "request log": logged.String()} {
		for _, form := range fake.forms() {
			if strings.Contains(text, form) {
				t.Errorf("%s contains key material (%q):\n%s", where, form, text)
			}
		}
	}
}

// The helper removes each form, keeps the error chain, and leaves an error
// with no secret untouched.
func TestRedactRemovesEveryFormAndKeepsTheChain(t *testing.T) {
	sec := []byte("an-example-secret-value-0123456789")
	inner := errors.New("kubectl: " + string(sec) + " " + base64.StdEncoding.EncodeToString(sec) + " " + hex.EncodeToString(sec))
	err := redact(fmt.Errorf("wrapped: %w", inner), sec)
	for _, form := range []string{string(sec), base64.StdEncoding.EncodeToString(sec), hex.EncodeToString(sec)} {
		if strings.Contains(err.Error(), form) {
			t.Errorf("redacted error still has %q: %v", form, err)
		}
	}
	if !errors.Is(err, inner) {
		t.Error("redact broke the error chain")
	}
	plain := errors.New("Unable to connect to the server")
	if got := redact(plain, sec); got != plain {
		t.Errorf("an error without a secret was changed: %v", got)
	}
}
