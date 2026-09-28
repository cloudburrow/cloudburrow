package bigqueryfront

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"time"
)

// ErrUsage means the arguments were refused; the reason has been written.
var ErrUsage = errors.New("usage")

// Run serves the front with the flags of `cloudburrow-storage
// bigquery-front` until ctx ends (#902). It runs in the BigQuery pod beside
// the emulator: it serves the Service's REST port and forwards what it
// passes to the emulator's, on the pod's loopback, so every client of the
// Service goes through it, the host's tunnel and any pod alike.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("bigquery-front", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "0.0.0.0:9050", "address the front serves BigQuery's REST API on")
	upstream := fs.String("upstream", "127.0.0.1:9051", "the BigQuery emulator's REST address")
	readListen := fs.String("storage-read-listen", "", "address the front serves the Storage Read API (gRPC) on, such as 0.0.0.0:9060; empty: not served (#1032)")
	readUpstream := fs.String("storage-read-upstream", "127.0.0.1:9061", "the BigQuery emulator's Storage Read API (gRPC) address")
	storage := fs.String("storage", "", "the instance's Cloud Storage (http://host:port), which a load's gs:// URIs are read from and an extract job's bucket is looked up in")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if fs.NArg() > 0 || *upstream == "" {
		fmt.Fprintln(stderr, "bigquery-front: unexpected arguments, or no --upstream")
		return ErrUsage
	}
	logger := log.New(stdout, "", log.LstdFlags|log.LUTC)
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *listen, err)
	}
	// Between the front and the emulator: query results written to one
	// dataset (results.go, #1017), then the guard against the engine's
	// failure (engine.go, #989). The watch drops what the front keeps
	// when the emulator restarts (restart.go, #1016).
	watch := newEmulatorWatch(*upstream, logger.Printf)
	results := Results(Guard(Proxy(*upstream, logger.Printf), *upstream, logger.Printf))
	watch.onRestart(results.reset)
	// The table IDs the REST front keeps (tableids.go, #1063), and its job
	// records, which leave the Storage Read front's queries out of
	// jobs.list (storagerows.go, #1095).
	ids, records := &tableIDs{}, &jobRecords{}
	go watch.run(ctx)
	go results.expire(ctx) // #1059
	var readL net.Listener
	if *readListen != "" {
		if readL, err = net.Listen("tcp", *readListen); err != nil {
			_ = l.Close()
			return fmt.Errorf("listen on %s: %w", *readListen, err)
		}
	}
	srv := &http.Server{
		Handler:           Wrap(results, WithStorage(*storage), func(o *options) { o.restarts, o.ids, o.records = watch, ids, records }),
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          log.New(stderr, "bigquery-front: ", log.LstdFlags|log.LUTC),
	}
	logger.Printf("bigquery front: serving %s for the emulator at %s", l.Addr(), *upstream)
	done := make(chan error, 2)
	go func() { done <- srv.Serve(l) }()
	readCtx, stopRead := context.WithCancel(ctx)
	defer stopRead()
	if readL != nil {
		logger.Printf("bigquery front: serving the Storage Read API on %s for the emulator's at %s", readL.Addr(), *readUpstream)
		go func() {
			err := serveStorageRead(readCtx, readL, *readUpstream, results, records)
			if err == nil && readCtx.Err() == nil {
				err = errors.New("the Storage Read API front stopped")
			}
			if readCtx.Err() == nil {
				done <- err
			}
		}()
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return nil
	}
}

// Proxy is the path from the front to the emulator at upstream: a reverse
// proxy that keeps the client's Host, as the emulator saw it before the
// front stood in its pod, and answers 502 when the emulator does not.
func Proxy(upstream string, logf func(string, ...any)) http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = upstream
			pr.Out.Host = pr.In.Host
		},
		// The emulator's bodies pass through as it wrote them.
		Transport: &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 32, IdleConnTimeout: 90 * time.Second},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			logf("bigquery front: %s %s: %v", r.Method, r.URL.Path, err)
			writeError(w, http.StatusBadGateway, "backendError", "cloudburrow: the BigQuery emulator did not answer: "+err.Error())
		},
	}
}
