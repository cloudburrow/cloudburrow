package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	kmsapi "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/metrics"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
)

// leakForms returns every form of b a surface could carry it in.
func leakForms(b []byte) []string {
	return []string{string(b), base64.StdEncoding.EncodeToString(b), base64.RawStdEncoding.EncodeToString(b),
		base64.URLEncoding.EncodeToString(b), base64.RawURLEncoding.EncodeToString(b), hex.EncodeToString(b)}
}

// TestKMSNeverEchoesPlaintextAADOrCiphertext (#417): with every surface
// CloudBurrow emits attached (the request log at trace, /admin/events,
// /metrics, OpenTelemetry spans and the error each client sees), the
// successful and failing Encrypt and Decrypt cases, over gRPC and REST,
// leave no trace of the plaintext, the AAD or the ciphertext.
func TestKMSNeverEchoesPlaintextAADOrCiphertext(t *testing.T) {
	var spanMu sync.Mutex
	var spanBytes bytes.Buffer
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		spanMu.Lock()
		spanBytes.Write(b)
		spanMu.Unlock()
	}))
	defer collector.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	tracing, err := telemetry.Setup(context.Background(), os.Getenv, "test")
	if err != nil {
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	logger := slog.New(grpctransport.NewLineHandler(lockedWriter{&logBuf, &logMu}, grpctransport.LevelTrace))
	rec := admin.NewRecorder(1000, nil)
	reg := metrics.New()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Services = []config.Service{config.ServiceKMS}
	cfg.Endpoints.KMS = 0
	svc := &kmsService{cfg: cfg, db: store.NewMemory(), tracing: tracing,
		calls: callEvents(rec, reg, "kms"), requests: requestEvents(rec, reg, "kms"),
		interpose: []grpc.UnaryServerInterceptor{grpctransport.LogInterceptor(logger, "kms")}}
	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer svc.Stop(ctx)
	g, err := kmsapi.NewKeyManagementClient(ctx, option.WithEndpoint(svc.Addr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	rc, err := kmsapi.NewKeyManagementRESTClient(ctx, option.WithEndpoint("http://"+svc.Addr()), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	pt, aad := []byte("PLAINTEXT-MARKER-7f3a9c"), []byte("AAD-MARKER-2b8e41")
	var secrets [][]byte
	secrets = append(secrets, pt, aad)
	var clientErrors []string
	for variant, c := range map[string]*kmsapi.KeyManagementClient{"grpc": g, "rest": rc} {
		ring, err := g.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/demo-project/locations/global", KeyRingId: "leak-" + variant})
		if err != nil {
			t.Fatal(err)
		}
		key, err := g.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
			CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
		if err != nil {
			t.Fatal(err)
		}
		enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: pt, AdditionalAuthenticatedData: aad})
		if err != nil {
			t.Fatalf("%s Encrypt: %v", variant, err)
		}
		ct := enc.GetCiphertext()
		secrets = append(secrets, ct)
		if _, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: ct, AdditionalAuthenticatedData: aad}); err != nil {
			t.Fatalf("%s Decrypt: %v", variant, err)
		}
		tampered := bytes.Clone(ct)
		tampered[len(tampered)-1] ^= 1
		failing := map[string]func() error{
			"wrong AAD": func() error {
				_, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: ct, AdditionalAuthenticatedData: []byte("AAD-MARKER-wrong")})
				return err
			},
			"tampered ciphertext": func() error {
				_, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: tampered, AdditionalAuthenticatedData: aad})
				return err
			},
			"CRC32C mismatch": func() error {
				_, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: pt, AdditionalAuthenticatedData: aad, PlaintextCrc32C: wrapperspb.Int64(1)})
				return err
			},
		}
		for what, call := range failing {
			if err := call(); err == nil {
				t.Errorf("%s %s succeeded", variant, what)
			} else {
				clientErrors = append(clientErrors, variant+" "+what+": "+err.Error())
			}
		}
		if _, err := g.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"state"}},
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key.GetPrimary().GetName(), State: kmspb.CryptoKeyVersion_DISABLED}}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: pt, AdditionalAuthenticatedData: aad}); err == nil {
			t.Errorf("%s Encrypt against a DISABLED primary succeeded", variant)
		} else {
			clientErrors = append(clientErrors, variant+" DISABLED primary: "+err.Error())
		}
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = tracing.Shutdown(sctx)

	events, _ := json.Marshal(rec.Events("kms", 1000))
	var metricsText bytes.Buffer
	_ = reg.Write(&metricsText)
	logMu.Lock()
	logText := logBuf.String()
	logMu.Unlock()
	spanMu.Lock()
	spanText := spanBytes.String()
	spanMu.Unlock()
	if logText == "" || len(events) < 10 || spanText == "" {
		t.Fatalf("a surface captured nothing: log %d bytes, events %d, spans %d", len(logText), len(events), len(spanText))
	}
	surfaces := map[string]string{"request log": logText, "/admin/events": string(events), "/metrics": metricsText.String(),
		"spans": spanText, "client errors": strings.Join(clientErrors, "\n")}
	for name, text := range surfaces {
		for _, s := range secrets {
			for _, form := range leakForms(s) {
				if len(form) >= 8 && strings.Contains(text, form) {
					t.Errorf("%s carries %q", name, form)
				}
			}
		}
	}
	// And every error names what was wrong without its value.
	for _, e := range clientErrors {
		if !strings.Contains(e, "crc32c") && !strings.Contains(e, "additional authenticated data") && !strings.Contains(e, "DISABLED") {
			t.Errorf("an error does not say what was wrong: %s", e)
		}
	}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
