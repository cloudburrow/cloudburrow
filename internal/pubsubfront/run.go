package pubsubfront

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"time"
)

// ErrUsage means the arguments were refused; the reason has been written.
var ErrUsage = errors.New("usage")

// Run serves the front with the flags of `cloudburrow-storage pubsub-front`
// until ctx ends.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pubsub-front", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "0.0.0.0:8085", "address the front serves Pub/Sub on")
	upstream := fs.String("upstream", "127.0.0.1:8086", "the Pub/Sub emulator's address")
	interval := fs.Duration("sweep-interval", 30*time.Second, "how often idle subscriptions are looked for")
	relay := fs.String("push-relay", "", "loopback address the push relay serves the emulator's pushes on, "+
		"such as 127.0.0.1:8087; empty: no relay, and push subscriptions never expire")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if fs.NArg() > 0 || *interval <= 0 {
		fmt.Fprintln(stderr, "pubsub-front: unexpected arguments, or a sweep interval that is not positive")
		return ErrUsage
	}
	logger := log.New(stdout, "", log.LstdFlags|log.LUTC)
	f, err := New(*upstream, logger.Printf)
	if err != nil {
		return err
	}
	defer f.Close()
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *listen, err)
	}
	if *relay != "" {
		rl, err := net.Listen("tcp", *relay)
		if err != nil {
			_ = l.Close()
			return fmt.Errorf("listen on %s: %w", *relay, err)
		}
		f.RelayPushes(ctx, rl)
		logger.Printf("pubsub front: relaying pushes through %s", rl.Addr())
	}
	logger.Printf("pubsub front: serving %s for the emulator at %s", l.Addr(), *upstream)
	return f.Serve(ctx, l, *interval)
}
