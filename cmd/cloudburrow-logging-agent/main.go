// Command cloudburrow-logging-agent is Kubernetes Engine's node logging
// agent (#2100): the kube-system/fluentbit-gke DaemonSet's container, which
// ships its node's container logs to the instance's Cloud Logging. The CLI
// embeds its Linux builds and loads the image it makes from them into each
// cluster's nodes; it is never published. See internal/logagent.
//
// It reads NODE_NAME (the downward API's spec.nodeName) and
// LOGGING_ENDPOINT (the instance's Cloud Logging, http://host:port), and
// refuses to start without either.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/logagent"
)

func main() {
	log.SetFlags(0)
	node := os.Getenv("NODE_NAME")
	if node == "" {
		log.Fatal("NODE_NAME is not set")
	}
	w, err := logagent.NewWriter(os.Getenv("LOGGING_ENDPOINT"))
	if err != nil {
		log.Fatal(err)
	}
	k, err := logagent.NewInCluster(node)
	if err != nil {
		log.Fatal(err)
	}
	root := envOr("LOG_ROOT", "/var/log/pods")
	posDir := envOr("POSITION_DIR", logagent.PositionDir)
	if err := os.MkdirAll(posDir, 0o700); err != nil {
		log.Fatal(err)
	}
	a := &logagent.Agent{
		Node: node, Root: root, Positions: filepath.Join(posDir, "positions.json"),
		Cluster: k, Write: w.Write, Every: time.Second,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
		},
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	_ = a.Run(ctx)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
