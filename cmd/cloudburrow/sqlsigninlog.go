package main

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// pgSignInLog follows the log of deployment's PostgreSQL server, the
// container's stderr, through the cluster (kubectl logs --follow
// --timestamps), for the lock after failed sign-ins (#2014): the server
// logs each failed password there, and only there. For a created
// instance's Deployment it is the primary's server, the one the
// instance's Service selects, which a failover changes (#1408); the stream
// ends when that pod goes, and the watcher follows again.
func pgSignInLog(cfg config.Config, deployment string, primary bool) sqladmin.SignInLog {
	kube := kubeRunner(cfg.KubeconfigPath(), cfg.Cluster.Namespace)
	return func(ctx context.Context, since time.Time) (io.ReadCloser, error) {
		d := deployment
		if primary {
			if app, err := kube.Get(ctx, "service", deployment, "jsonpath={.spec.selector.app}"); err == nil &&
				strings.TrimSpace(app) != "" {
				d = strings.TrimSpace(app)
			}
		}
		out, wait, err := kube.Stream(ctx, "logs", "--follow", "--timestamps",
			"--since-time="+since.UTC().Format(time.RFC3339), "deployment/"+d)
		if err != nil {
			return nil, err
		}
		return &streamCloser{ReadCloser: out, wait: wait}, nil
	}
}

// streamCloser reaps kubectl once its stdout is closed.
type streamCloser struct {
	io.ReadCloser
	wait func() error
}

func (s *streamCloser) Close() error {
	err := s.ReadCloser.Close()
	_ = s.wait()
	return err
}
