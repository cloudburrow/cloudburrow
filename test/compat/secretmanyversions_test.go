//go:build compat

package compat

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
)

// TestSecretTwoThousandVersions adds 2,000 versions to one secret through the
// official client (#1596). Until #1596 every version's metadata was an
// annotation on the one Kubernetes Secret behind the secret, and Kubernetes'
// 256 KiB cap on annotations made the 905th AddSecretVersion INTERNAL; and
// every call ran kubectl get on that ever larger object, so latency grew with
// the count. Secret Manager documents no cap on versions
// (docs.cloud.google.com/secret-manager/quotas). Now each version is a
// Kubernetes Secret of its own and reads are answered from memory: the first
// and the last versions read back, and the p99 of the last 200 adds and
// accesses is within twice that of the first 200.
// covers: google.cloud.secretmanager.v1.SecretManagerService/AddSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/AccessSecretVersion
func TestSecretTwoThousandVersions(t *testing.T) {
	h := New(t)
	c := secretsClient(t, h)
	id := fmt.Sprintf("compat-many-versions-%d", time.Now().UnixNano())
	name := secretsParent(h) + "/secrets/" + id
	if _, err := c.CreateSecret(h.Context(), &secretmanagerpb.CreateSecretRequest{
		Parent: secretsParent(h), SecretId: id,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}},
	}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = c.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: name})
	})

	const total, window = 2000, 200
	call := func(f func(ctx context.Context) error) (time.Duration, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		start := time.Now()
		err := f(ctx)
		return time.Since(start), err
	}
	var adds, accesses []time.Duration
	for i := 1; i <= total; i++ {
		d, err := call(func(ctx context.Context) error {
			_, err := c.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
				Parent:  name,
				Payload: &secretmanagerpb.SecretPayload{Data: []byte(fmt.Sprintf("payload-%04d", i))},
			})
			return err
		})
		if err != nil {
			t.Fatalf("AddSecretVersion %d: %v", i, err)
		}
		adds = append(adds, d)
		if i <= window || i > total-window {
			d, err := call(func(ctx context.Context) error {
				_, err := c.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name + "/versions/latest"})
				return err
			})
			if err != nil {
				t.Fatalf("AccessSecretVersion latest at %d: %v", i, err)
			}
			accesses = append(accesses, d)
		}
	}

	for n, want := range map[int]string{1: "payload-0001", total: fmt.Sprintf("payload-%04d", total)} {
		got, err := c.AccessSecretVersion(h.Context(), &secretmanagerpb.AccessSecretVersionRequest{
			Name: fmt.Sprintf("%s/versions/%d", name, n)})
		if err != nil {
			t.Fatalf("AccessSecretVersion %d: %v", n, err)
		}
		if string(got.GetPayload().GetData()) != want {
			t.Errorf("version %d = %q, want %q", n, got.GetPayload().GetData(), want)
		}
	}

	p99 := func(ds []time.Duration) time.Duration {
		s := append([]time.Duration(nil), ds...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		return s[(len(s)*99)/100]
	}
	for _, m := range []struct {
		what       string
		start, end []time.Duration
	}{
		{"AddSecretVersion", adds[:window], adds[total-window:]},
		{"AccessSecretVersion latest", accesses[:window], accesses[window:]},
	} {
		first, last := p99(m.start), p99(m.end)
		t.Logf("%s p99: first %d calls %v, last %d %v", m.what, window, first, window, last)
		if last > 2*first {
			t.Errorf("%s p99 grew from %v to %v with the version count", m.what, first, last)
		}
	}
}
