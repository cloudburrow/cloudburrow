package main

import (
	"context"
	"crypto/rsa"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	pubsub "cloud.google.com/go/pubsub/v2"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/service/storage"
)

// runStorageServer is `cloudburrow storage-server`: the builtin Cloud Storage
// server on its own (#488). It binds loopback unless --allow-remote says
// otherwise, as every CloudBurrow endpoint does (ADR-0004).
func runStorageServer(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("storage-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:4443", "address to serve on")
	hosts := fs.String("host", "", "comma-separated host names for virtual-hosted XML requests")
	allowRemote := fs.Bool("allow-remote", false, "permit a non-loopback listen address")
	dataDir := fs.String("data-dir", "", "directory to keep state in (default: memory only)")
	var certs signingCertFlag
	fs.Var(&certs, "signing-cert", "email=path.pem: a public certificate or key to verify that service account's signed URLs against (repeatable)")
	pubsubAddr := fs.String("pubsub-emulator", "", "host:port of a Pub/Sub emulator to deliver notifications to (default: notifications cannot be configured)")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("--listen %q: %w", *listen, err)
	}
	if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && host != "localhost" && !*allowRemote {
		return fmt.Errorf("--listen %s is not a loopback address; it would expose an unauthenticated server, so pass --allow-remote to confirm", *listen)
	}
	var names []string
	for _, h := range strings.Split(*hosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			names = append(names, h)
		}
	}
	opts := storage.Options{Hosts: names}
	if *dataDir != "" {
		meta, err := storage.OpenLogMetaStore(filepath.Join(*dataDir, "meta"))
		if err != nil {
			return err
		}
		defer meta.Close()
		blobs, err := storage.OpenFileBlobStore(filepath.Join(*dataDir, "objects"), storage.Limits{})
		if err != nil {
			return err
		}
		opts.Meta, opts.Blobs = meta, blobs
	}
	if len(certs) > 0 {
		opts.SigningKeys = map[string]*rsa.PublicKey{}
		for email, path := range certs {
			raw, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("--signing-cert %s: %w", email, err)
			}
			k, err := storage.ParseSigningKey(raw)
			if err != nil {
				return fmt.Errorf("--signing-cert %s=%s: %w", email, path, err)
			}
			opts.SigningKeys[email] = k
		}
	}
	if *pubsubAddr != "" {
		pub, err := newEmulatorPublisher(ctx, *pubsubAddr)
		if err != nil {
			return err
		}
		defer pub.Close()
		opts.Publisher = pub
	}
	srv, err := storage.NewServer(opts)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 30 * time.Second}
	fmt.Fprintf(stdout, "Cloud Storage (builtin, in development) listening on http://%s\n", ln.Addr())
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	// Soft-deleted objects and buckets are removed at their hardDeleteTime
	// (#499); a sweep at start catches what fell due while it was stopped.
	sweepCtx, stopSweep := context.WithCancel(context.Background())
	defer stopSweep()
	go srv.Run(sweepCtx, sched.RealClock{})
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := hs.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// emulatorPublisher publishes notifications to a Pub/Sub emulator (#506),
// over plaintext gRPC with no credentials, as every emulator client does.
type emulatorPublisher struct {
	client *pubsub.Client
}

func newEmulatorPublisher(ctx context.Context, addr string) (*emulatorPublisher, error) {
	// The client's project only names the client; every publish names its
	// topic in full.
	c, err := pubsub.NewClient(ctx, "cloudburrow-storage",
		option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return nil, fmt.Errorf("--pubsub-emulator %s: %w", addr, err)
	}
	return &emulatorPublisher{client: c}, nil
}

func (p *emulatorPublisher) Publish(ctx context.Context, topic string, data []byte, attrs map[string]string) error {
	pubCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pb := p.client.Publisher(topic)
	defer pb.Stop()
	_, err := pb.Publish(pubCtx, &pubsub.Message{Data: data, Attributes: attrs}).Get(pubCtx)
	return err
}

func (p *emulatorPublisher) Close() { _ = p.client.Close() }

// signingCertFlag collects --signing-cert email=path.pem (#509).
type signingCertFlag map[string]string

func (f *signingCertFlag) String() string { return fmt.Sprint(map[string]string(*f)) }

func (f *signingCertFlag) Set(v string) error {
	email, path, ok := strings.Cut(v, "=")
	if !ok || !strings.Contains(email, "@") || path == "" {
		return fmt.Errorf("want email=path.pem, not %q", v)
	}
	if *f == nil {
		*f = signingCertFlag{}
	}
	(*f)[email] = path
	return nil
}
