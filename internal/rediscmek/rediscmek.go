// Package rediscmek keeps a Memorystore for Redis server's persistence
// files encrypted with its customer-managed key (#2113), as
// `cloudburrow-storage memorystore-cmek`.
//
// Memorystore documents that with CMEK "customer data that are stored in
// persistent storage", the RDB snapshots and the AOF among them, are
// encrypted with the key (docs re-read 2026-11-21:
// https://docs.cloud.google.com/memorystore/docs/redis/cmek). Valkey writes
// its files itself, in the clear, so the server's directory is a memory
// volume (an emptyDir of medium Memory) and its persistent volume holds
// only their sealed copies: each file sealed with the instance's 256-bit
// data key (AES-256-GCM), the data key kept only as Cloud KMS wrapped it.
//
// Run, the server container's command, asks the instance's Cloud KMS to
// Decrypt the wrapped data key, so a key whose version is disabled or
// destroyed really refuses it and the server does not start; opens the
// sealed files into the memory directory; starts the server; seals every
// file that changed every interval and once more when the server exits
// (its shutdown snapshot among them), each written beside its old copy and
// renamed over it; and passes SIGTERM and SIGINT on to the server. It asks
// Cloud KMS again every --check: while the key cannot be used it drops the
// data key and seals nothing more, so nothing more reaches the volume (a
// Redis cluster's or Valkey instance's persistence is deactivated, as
// Memorystore documents), and it seals again once the key can be used.
package rediscmek

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ErrUsage means the arguments were refused; the reason has been written.
var ErrUsage = errors.New("usage")

// EnvWrapped is the variable that carries the wrapped data key, base64.
const EnvWrapped = "MEMORYSTORE_CMEK_WRAPPED_KEY"

// magic starts every sealed file.
var magic = []byte("CBCMEK1\n")

// Suffix ends every sealed file's name.
const Suffix = ".sealed"

// Decrypter is Cloud KMS's Decrypt.
type Decrypter interface {
	Decrypt(ctx context.Context, in *kmspb.DecryptRequest, opts ...grpc.CallOption) (*kmspb.DecryptResponse, error)
}

// Run is `memorystore-cmek [flags] -- command...`.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("memorystore-cmek", flag.ContinueOnError)
	fs.SetOutput(stderr)
	plain := fs.String("plain", "", "the server's directory, on a memory volume")
	sealed := fs.String("sealed", "", "where the sealed files are kept, on the persistent volume")
	key := fs.String("key", "", "the CryptoKey that wrapped the data key")
	kmsAddr := fs.String("kms", "", "host:port of the instance's Cloud KMS (gRPC)")
	every := fs.Duration("every", time.Second, "how often changed files are sealed")
	forget := fs.Bool("forget", false, "drop the sealed files first: persistence is DISABLED")
	check := fs.Duration("check", 5*time.Second, "how often the key is checked again with Cloud KMS")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	cmd := fs.Args()
	if *plain == "" || *sealed == "" || *key == "" || *kmsAddr == "" || len(cmd) == 0 {
		fmt.Fprintln(stderr, "usage: memorystore-cmek --plain DIR --sealed DIR --key KEY --kms HOST:PORT -- command...")
		return ErrUsage
	}
	wrapped, err := base64.StdEncoding.DecodeString(os.Getenv(EnvWrapped))
	if err != nil || len(wrapped) == 0 {
		return fmt.Errorf("%s holds no wrapped data key", EnvWrapped)
	}
	conn, err := grpc.NewClient(*kmsAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	kms := kmspb.NewKeyManagementServiceClient(conn)
	dek, err := Unwrap(ctx, kms, *key, wrapped)
	if err != nil {
		return err
	}
	k := &Keeper{Plain: *plain, Sealed: *sealed, DEK: dek}
	if *forget {
		if err := Forget(*sealed); err != nil {
			return err
		}
	}
	if err := k.Open(); err != nil {
		return err
	}
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, stdout, stderr
	if err := c.Start(); err != nil {
		return err
	}
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	t := time.NewTicker(*every)
	defer t.Stop()
	kt := time.NewTicker(*check)
	defer kt.Stop()
	// lost is whether the key could not be used at its last check: the
	// data key is then dropped and nothing more is sealed, so nothing more
	// is written to the volume, until the key can be used again.
	lost := false
	for {
		select {
		case s := <-sigs:
			_ = c.Process.Signal(s)
		case <-kt.C:
			d, err := Unwrap(ctx, kms, *key, wrapped)
			switch {
			case err != nil && !lost:
				fmt.Fprintln(stderr, "memorystore-cmek: the key can no longer be used; nothing more is sealed:", err)
				lost, k.DEK = true, nil
			case err == nil && lost:
				fmt.Fprintln(stderr, "memorystore-cmek: the key can be used again")
				lost, k.DEK = false, d
			}
		case <-t.C:
			if lost {
				continue
			}
			if err := k.Seal(); err != nil {
				fmt.Fprintln(stderr, "memorystore-cmek: seal:", err)
			}
		case err := <-done:
			if !lost {
				if serr := k.Seal(); serr != nil {
					fmt.Fprintln(stderr, "memorystore-cmek: seal:", serr)
				}
			}
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				os.Exit(ee.ExitCode())
			}
			return err
		}
	}
}

// Forget removes everything under dir, which stays: it is the persistent
// volume's mount point, which cannot itself be removed.
func Forget(dir string) error {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Unwrap asks Cloud KMS to Decrypt the wrapped data key.
func Unwrap(ctx context.Context, kms Decrypter, key string, wrapped []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := kms.Decrypt(ctx, &kmspb.DecryptRequest{Name: key, Ciphertext: wrapped})
	if err != nil {
		return nil, fmt.Errorf("the customer-managed key %s cannot decrypt the instance's data key: %w", key, err)
	}
	if len(r.GetPlaintext()) != 32 {
		return nil, fmt.Errorf("the instance's data key is %d bytes, not 32", len(r.GetPlaintext()))
	}
	return r.GetPlaintext(), nil
}

// Keeper seals the files under Plain into Sealed with DEK.
type Keeper struct {
	Plain, Sealed string
	DEK           []byte
	// seen is each file's size and modification time when last sealed.
	seen map[string]string
}

func (k *Keeper) aead() (cipher.AEAD, error) {
	b, err := aes.NewCipher(k.DEK)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// SealBytes is one file's sealed form.
func (k *Keeper) SealBytes(name string, plain []byte) ([]byte, error) {
	g, err := k.aead()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append(append([]byte{}, magic...), nonce...)
	return g.Seal(out, nonce, plain, []byte(name)), nil
}

// OpenBytes is a sealed file's contents.
func (k *Keeper) OpenBytes(name string, sealed []byte) ([]byte, error) {
	g, err := k.aead()
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(sealed, magic) || len(sealed) < len(magic)+g.NonceSize() {
		return nil, fmt.Errorf("%s is not a sealed file", name)
	}
	rest := sealed[len(magic):]
	return g.Open(nil, rest[:g.NonceSize()], rest[g.NonceSize():], []byte(name))
}

// Open writes every sealed file, opened, under Plain.
func (k *Keeper) Open() error {
	if err := os.MkdirAll(k.Plain, 0o777); err != nil {
		return err
	}
	_ = os.Chmod(k.Plain, 0o777)
	if err := os.MkdirAll(k.Sealed, 0o700); err != nil {
		return err
	}
	k.seen = map[string]string{}
	return filepath.WalkDir(k.Sealed, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, Suffix) {
			return err
		}
		rel, _ := filepath.Rel(k.Sealed, strings.TrimSuffix(p, Suffix))
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		plain, err := k.OpenBytes(rel, raw)
		if err != nil {
			return fmt.Errorf("open %s: %w", rel, err)
		}
		dst := filepath.Join(k.Plain, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
			return err
		}
		_ = os.Chmod(filepath.Dir(dst), 0o777)
		if err := os.WriteFile(dst, plain, 0o666); err != nil {
			return err
		}
		_ = os.Chmod(dst, 0o666)
		if st, err := os.Stat(dst); err == nil {
			k.seen[rel] = stamp(st)
		}
		return nil
	})
}

func stamp(st fs.FileInfo) string { return fmt.Sprintf("%d/%d", st.Size(), st.ModTime().UnixNano()) }

// Seal seals every file under Plain that changed since it was last
// sealed, and drops the sealed copies of files no longer there.
func (k *Keeper) Seal() error {
	if k.seen == nil {
		k.seen = map[string]string{}
	}
	present := map[string]bool{}
	err := filepath.WalkDir(k.Plain, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := d.Name()
		// Valkey's files being written (temp-*.rdb, temp-rewriteaof-*)
		// are sealed once renamed.
		if strings.HasPrefix(base, "temp-") {
			return nil
		}
		rel, _ := filepath.Rel(k.Plain, p)
		present[rel] = true
		st, err := d.Info()
		if err != nil {
			return nil
		}
		if k.seen[rel] == stamp(st) {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		out, err := k.SealBytes(rel, raw)
		if err != nil {
			return err
		}
		dst := filepath.Join(k.Sealed, rel) + Suffix
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		tmp := dst + ".tmp"
		if err := os.WriteFile(tmp, out, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, dst); err != nil {
			return err
		}
		k.seen[rel] = stamp(st)
		return nil
	})
	if err != nil {
		return err
	}
	return filepath.WalkDir(k.Sealed, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, Suffix) {
			return err
		}
		rel, _ := filepath.Rel(k.Sealed, strings.TrimSuffix(p, Suffix))
		if !present[rel] {
			delete(k.seen, rel)
			return os.Remove(p)
		}
		return nil
	})
}
