package bigtablefront

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
)

// ErrUsage means the arguments were refused; the reason has been written.
var ErrUsage = errors.New("usage")

// Run serves the front with the flags of `cloudburrow-storage bigtable-front`
// until ctx ends.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("bigtable-front", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "0.0.0.0:8086", "address the front serves Bigtable on")
	upstream := fs.String("upstream", "127.0.0.1:8096", "the Bigtable emulator's address")
	// Where the front keeps its records and data copies (state.go,
	// #1514): the pod's emptyDir.
	stateDir := fs.String("state-dir", "", "directory the front keeps its state in")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "bigtable-front: unexpected arguments")
		return ErrUsage
	}
	logger := log.New(stdout, "", log.LstdFlags|log.LUTC)
	f, err := New(*upstream, logger.Printf)
	if err != nil {
		return err
	}
	defer f.Close()
	if *stateDir != "" {
		if err := f.KeepState(ctx, *stateDir); err != nil {
			return fmt.Errorf("the front's state: %w", err)
		}
	}
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *listen, err)
	}
	logger.Printf("bigtable front: serving %s for the emulator at %s", l.Addr(), *upstream)
	return f.Serve(ctx, l)
}
