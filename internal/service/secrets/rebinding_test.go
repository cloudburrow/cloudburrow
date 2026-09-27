package secrets

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The Secret Manager port refuses a rebound attacker domain on its JSON API
// (#676), which would otherwise hand a web page every secret value, and
// still serves gRPC, whose :authority is whatever name the client dialed and
// which a browser cannot send.
func TestSecretManagerPortRefusesForeignHosts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv := NewServer("127.0.0.1:0", newTestStore(t))
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop(ctx) })

	conn, err := grpc.NewClient(srv.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithAuthority("some-name.example:9006"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := secretmanagerpb.NewSecretManagerServiceClient(conn).CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/demo", SecretId: "k",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}},
	}); err != nil {
		t.Fatalf("gRPC CreateSecret with a foreign :authority: %v", err)
	}

	get := func(host string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://"+srv.Addr()+"/v1/projects/demo/secrets", nil)
		req.Host = host
		req.Header.Set("Origin", "http://"+host)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("attacker.example:9006"); code != http.StatusMisdirectedRequest || !strings.Contains(body, "attacker.example:9006") {
		t.Errorf("JSON list with Host attacker.example:9006 = %d %q, want 421 naming the host", code, body)
	}
	for _, host := range []string{"127.0.0.1:9006", "localhost:9006", "cloudburrow-host.cloudburrow.svc.cluster.local:9006", "host.docker.internal:9006"} {
		if code, body := get(host); code != http.StatusOK || !strings.Contains(body, "projects/demo/secrets/k") {
			t.Errorf("JSON list with Host %s = %d %.200s, want the secret", host, code, body)
		}
	}
}
