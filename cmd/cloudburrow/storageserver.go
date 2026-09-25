package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

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
	srv, err := storage.NewServer(names...)
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
