package rediscmek

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc"
)

// TestKeeperSealsAndOpens: a file the server writes is sealed on the
// persistent volume, never in the clear there, and opened again into a
// new memory directory; a file removed loses its sealed copy; another
// data key cannot open it.
func TestKeeperSealsAndOpens(t *testing.T) {
	dir := t.TempDir()
	dek := bytes.Repeat([]byte{7}, 32)
	k := &Keeper{Plain: filepath.Join(dir, "plain"), Sealed: filepath.Join(dir, "sealed"), DEK: dek}
	if err := k.Open(); err != nil {
		t.Fatal(err)
	}
	secret := []byte("REDIS0011 the key's value: hunter2")
	if err := os.MkdirAll(filepath.Join(k.Plain, "appendonlydir"), 0o777); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"dump.rdb", "appendonlydir/appendonly.aof.1.incr.aof"} {
		if err := os.WriteFile(filepath.Join(k.Plain, f), secret, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(k.Plain, "temp-123.rdb"), secret, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := k.Seal(); err != nil {
		t.Fatal(err)
	}
	var sealed []string
	_ = filepath.Walk(k.Sealed, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			raw, _ := os.ReadFile(p)
			if bytes.Contains(raw, []byte("hunter2")) {
				t.Errorf("%s holds the value in the clear", p)
			}
			rel, _ := filepath.Rel(k.Sealed, p)
			sealed = append(sealed, rel)
		}
		return nil
	})
	if len(sealed) != 2 {
		t.Errorf("sealed files = %v, want the RDB and the AOF, not the temp file", sealed)
	}
	again := &Keeper{Plain: filepath.Join(dir, "plain2"), Sealed: k.Sealed, DEK: dek}
	if err := again.Open(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(again.Plain, "appendonlydir/appendonly.aof.1.incr.aof"))
	if err != nil || !bytes.Equal(got, secret) {
		t.Errorf("opened AOF = %q, %v", got, err)
	}
	if err := os.Remove(filepath.Join(k.Plain, "dump.rdb")); err != nil {
		t.Fatal(err)
	}
	if err := k.Seal(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(k.Sealed, "dump.rdb"+Suffix)); !os.IsNotExist(err) {
		t.Errorf("the removed file's sealed copy = %v, want it gone", err)
	}
	other := &Keeper{Plain: filepath.Join(dir, "plain3"), Sealed: k.Sealed, DEK: bytes.Repeat([]byte{8}, 32)}
	if err := other.Open(); err == nil {
		t.Error("another data key opened the files")
	}
}

type fakeKMS struct{ err error }

func (f fakeKMS) Decrypt(_ context.Context, in *kmspb.DecryptRequest, _ ...grpc.CallOption) (*kmspb.DecryptResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &kmspb.DecryptResponse{Plaintext: bytes.Repeat([]byte{1}, 32)}, nil
}

// TestUnwrapRefusedKey: a key Cloud KMS refuses gives no data key.
func TestUnwrapRefusedKey(t *testing.T) {
	if _, err := Unwrap(context.Background(), fakeKMS{err: errors.New("FAILED_PRECONDITION: version is DISABLED")}, "k", []byte("w")); err == nil {
		t.Fatal("a refused Decrypt unwrapped the key")
	}
	if dek, err := Unwrap(context.Background(), fakeKMS{}, "k", []byte("w")); err != nil || len(dek) != 32 {
		t.Fatalf("Unwrap = %d bytes, %v", len(dek), err)
	}
}
