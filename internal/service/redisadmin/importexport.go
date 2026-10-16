package redisadmin

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Import and export (#1384) move RDB files between a created instance's
// server and this CloudBurrow instance's Cloud Storage, as Memorystore
// documents: ExportInstance writes a snapshot (BGSAVE) of the data to a
// gs:// object, and ImportInstance replaces the data with an RDB file's.
//
// An import is loaded by the server itself: the file is copied into its
// container, checked with the server's own check-rdb, and served there by
// a second server process of the same version on a loopback port, which
// the instance's server replicates once: a full sync empties its data and
// loads the file, as an import overwrites, and an AOF, when the server
// keeps one, is written again from it. A file of a newer RDB version than
// the instance's server reads is refused by check-rdb, as Memorystore
// imports a file of the instance's version or older.

// Shell runs a program in a created instance's server's container: the
// file copies and server processes import and export need.
type Shell interface {
	Exec(ctx context.Context, server string, stdin []byte, argv ...string) ([]byte, error)
}

// Objects reads and writes objects in this instance's Cloud Storage.
type Objects interface {
	Read(ctx context.Context, bucket, object string) ([]byte, error)
	Write(ctx context.Context, bucket, object string, data []byte) error
}

// UseStorage makes ImportInstance and ExportInstance work, over objs:
// without it they say Cloud Storage is not enabled.
func (s *Server) UseStorage(objs Objects) {
	if s.created != nil {
		s.created.objs = objs
	}
}

// importOpTimeout bounds an import or export.
var importOpTimeout = 30 * time.Minute

const (
	importDir  = "/tmp"
	importFile = "cloudburrow-import.rdb"
	importPort = 6380
)

// parseGCS splits gs://bucket/object.
func parseGCS(field, uri string) (bucket, object string, err error) {
	rest, ok := strings.CutPrefix(uri, "gs://")
	b, o, _ := strings.Cut(rest, "/")
	if !ok || b == "" || o == "" {
		return "", "", apierror.InvalidArgument("%s must be gs://{bucket}/{object}, not %q", field, uri)
	}
	return b, o, nil
}

// ioTarget is an instance an import or export runs on: a created one,
// READY, with nothing else in progress, on a server that can run them.
func (s *Server) ioTarget(name, verb string) (*instances, Shell, error) {
	isDefault, err := s.find(name)
	if err != nil {
		return nil, nil, err
	}
	if isDefault {
		return nil, nil, apierror.FailedPrecondition("instance %s is the server `cloudburrow up` runs, which cannot be %sed; create an instance to %s one", name, verb, verb)
	}
	c := s.created
	if c.objs == nil {
		return nil, nil, apierror.FailedPrecondition("%sing reads and writes this instance's Cloud Storage, which is not enabled: start it with --services memorystore,storage", verb)
	}
	sh, ok := c.prov.(Shell)
	if !ok {
		return nil, nil, apierror.Unimplemented("%sing is not supported by this instance's servers", verb)
	}
	return c, sh, nil
}

// claim marks r busy with a new operation, when it is READY and idle.
// Call with mu held.
func (c *instances) claim(r *record, verb string) (string, error) {
	if _, pending := c.busy[r.Name]; pending || r.state != redispb.Instance_READY {
		return "", apierror.FailedPrecondition("instance %s is %s or has an operation in progress: it can be %sed only when READY", r.Name, r.state, verb)
	}
	op := c.newOperation(r.Name, verb)
	c.busy[r.Name] = op
	return op, nil
}

// ExportInstance writes a snapshot of a created instance's data to a
// gs:// object, as an RDB file.
func (s *Server) ExportInstance(ctx context.Context, req *redispb.ExportInstanceRequest) (*longrunningpb.Operation, error) {
	bucket, object, err := parseGCS("output_config.gcs_destination.uri", req.GetOutputConfig().GetGcsDestination().GetUri())
	if err != nil {
		return nil, err
	}
	c, sh, err := s.ioTarget(req.GetName(), "export")
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	r := c.byID[req.GetName()]
	op, err := c.claim(r, "export")
	addr, auth, server := r.addr, r.access(), r.dataServer()
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	go func() {
		ctx, cancel := c.cancellable(op)
		defer cancel()
		data, err := c.snapshot(ctx, sh, server, addr, auth)
		if err == nil {
			err = c.objs.Write(ctx, bucket, object, data)
		}
		c.done(r.Name, op, redispb.Instance_READY, err)
	}()
	return s.operation(op)
}

// snapshot is the server's data as an RDB file: BGSAVE, then the file.
func (c *instances) snapshot(ctx context.Context, sh Shell, server, addr, auth string) ([]byte, error) {
	file, err := c.dialKey(addr, auth).Save(ctx)
	if err != nil {
		return nil, err
	}
	return sh.Exec(ctx, server, nil, "cat", file)
}

// ImportInstance replaces a created instance's data with an RDB file's,
// read from a gs:// object. The instance is IMPORTING until it is done.
func (s *Server) ImportInstance(ctx context.Context, req *redispb.ImportInstanceRequest) (*longrunningpb.Operation, error) {
	bucket, object, err := parseGCS("input_config.gcs_source.uri", req.GetInputConfig().GetGcsSource().GetUri())
	if err != nil {
		return nil, err
	}
	c, sh, err := s.ioTarget(req.GetName(), "import")
	if err != nil {
		return nil, err
	}
	// The object is read first, so a missing one is the call's error.
	data, err := c.objs.Read(ctx, bucket, object)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	r := c.byID[req.GetName()]
	op, err := c.claim(r, "import")
	if err == nil {
		r.state = redispb.Instance_IMPORTING
	}
	addr, auth, server, bin := r.addr, r.access(), r.dataServer(), r.binary()
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	go func() {
		ctx, cancel := c.cancellable(op)
		defer cancel()
		c.done(r.Name, op, redispb.Instance_READY, c.load(ctx, sh, server, bin, addr, auth, data))
	}()
	return s.operation(op)
}

// load replaces the data of the server at addr with the RDB file data.
func (c *instances) load(ctx context.Context, sh Shell, server, bin, addr, auth string, data []byte) error {
	file := importDir + "/" + importFile
	if _, err := sh.Exec(ctx, server, data, "sh", "-c", "cat > "+file); err != nil {
		return fmt.Errorf("copy the file to the server: %w", err)
	}
	cleanup := context.WithoutCancel(ctx)
	defer sh.Exec(cleanup, server, nil, "rm", "-f", file)
	if out, err := sh.Exec(ctx, server, nil, bin+"-check-rdb", file); err != nil {
		return apierror.InvalidArgument("the file is not an RDB file this instance's server can load: %s", lastLine(out, err))
	}
	port := strconv.Itoa(importPort)
	if out, err := sh.Exec(ctx, server, nil, bin+"-server", "--port", port, "--bind", "127.0.0.1", "--protected-mode", "no",
		"--dir", importDir, "--dbfilename", importFile, "--save", "", "--appendonly", "no",
		"--repl-diskless-sync", "yes", "--repl-diskless-sync-delay", "0", "--daemonize", "yes"); err != nil {
		return fmt.Errorf("start the server that serves the file: %s", lastLine(out, err))
	}
	defer sh.Exec(cleanup, server, nil, bin+"-cli", "-p", port, "shutdown", "nosave")
	for {
		out, _ := sh.Exec(ctx, server, nil, bin+"-cli", "-p", port, "ping")
		if strings.TrimSpace(string(out)) == "PONG" {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the server did not load the file: %w", ctx.Err())
		case <-time.After(savePoll):
		}
	}
	return c.dialKey(addr, auth).Load(ctx, "127.0.0.1", importPort)
}

// lastLine is the last line of a program's output, or its error.
func lastLine(out []byte, err error) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if l := strings.TrimSpace(lines[len(lines)-1]); l != "" {
		return l
	}
	return err.Error()
}

// done ends name's operation op: the instance back to state, and the
// operation done with it or failed with err.
func (c *instances) done(name, op string, state redispb.Instance_State, err error) {
	c.mu.Lock()
	delete(c.busy, name)
	r, ok := c.byID[name]
	var inst *redispb.Instance
	if ok {
		r.state = state
		inst = c.render(r)
	}
	c.mu.Unlock()
	if err != nil {
		var e *apierror.Error
		if errors.Is(err, context.Canceled) {
			e = &apierror.Error{Code: codes.Canceled, Message: "the operation was cancelled", Cause: err}
		} else if !errors.As(err, &e) {
			e = &apierror.Error{Code: codes.Internal, Message: err.Error(), Cause: err}
		}
		_ = c.ops.Fail(op, e)
		return
	}
	c.finish(op, inst)
}

// cancellable is the context an import or export runs under, bounded by
// importOpTimeout, which CancelOperation of op cancels.
func (c *instances) cancellable(op string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(c.bg, importOpTimeout)
	c.mu.Lock()
	c.cancels[op] = cancel
	c.mu.Unlock()
	return ctx, func() {
		c.mu.Lock()
		delete(c.cancels, op)
		c.mu.Unlock()
		cancel()
	}
}

// cancel cancels op, when it is an import or export in progress.
func (c *instances) cancel(op string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.cancels[op]
	if ok {
		f()
	}
	return ok
}

// access is the AUTH string and CA of r, as snapshot and load take them.
func (r *record) access() string { return r.AuthString + "\x00" + r.caPEM() }

// dialKey dials addr with an access key.
func (c *instances) dialKey(addr, key string) DataPlane {
	auth, ca, _ := strings.Cut(key, "\x00")
	return c.dial(addr, auth, ca)
}
