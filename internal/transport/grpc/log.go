package grpc

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"

	rpccode "google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Request logging (#314): one uniform line per request,
//
//	<service>.<Method> => <CODE> (<message>)
//
// at info for a call that failed and at debug for every call. At trace, the
// request's metadata is logged too, with the credential-bearing headers
// removed; bodies are never logged, so no secret payload can reach a log.

// LevelTrace is below debug: request metadata, for when a debug line is not
// enough to see what a client sent.
const LevelTrace = slog.LevelDebug - 4

// ParseLevel maps the configuration's level names to slog levels.
func ParseLevel(name string) (slog.Level, error) {
	switch name {
	case "trace":
		return LevelTrace, nil
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q", name)
}

// LineHandler writes "LEVEL message key=value..." lines: the message first,
// because it is the line a reader greps for, and no timestamp, because up.log
// stamps every line already.
type LineHandler struct {
	mu    *sync.Mutex
	w     io.Writer
	level slog.Leveler
	attrs []slog.Attr
}

// NewLineHandler returns a handler writing to w at level and above.
func NewLineHandler(w io.Writer, level slog.Leveler) *LineHandler {
	return &LineHandler{mu: &sync.Mutex{}, w: w, level: level}
}

func (h *LineHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level.Level() }

func levelName(l slog.Level) string {
	if l <= LevelTrace {
		return "TRACE"
	}
	return l.String()
}

func (h *LineHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%-5s %s", levelName(r.Level), r.Message)
	write := func(a slog.Attr) bool { fmt.Fprintf(&b, " %s=%v", a.Key, a.Value); return true }
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(write)
	b.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *LineHandler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr(nil), h.attrs...), as...)
	return &c
}

func (h *LineHandler) WithGroup(string) slog.Handler { return h }

// redacted are metadata keys whose values are never logged.
var redacted = map[string]bool{
	"authorization": true, "metadata-flavor": true, "x-goog-api-key": true,
	"x-goog-iam-authorization-token": true, "cookie": true, "proxy-authorization": true,
}

// LogInterceptor logs each call on service through logger.
func LogInterceptor(logger *slog.Logger, service string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if logger == nil {
			return resp, err
		}
		method := info.FullMethod[strings.LastIndex(info.FullMethod, "/")+1:]
		st := status.Convert(err)
		line := fmt.Sprintf("%s.%s => %s", service, method, canonical(st.Code()))
		if err != nil {
			line += " (" + st.Message() + ")"
		}
		level := slog.LevelDebug
		if err != nil {
			level = slog.LevelInfo
		}
		logger.Log(ctx, level, line)
		if logger.Enabled(ctx, LevelTrace) {
			if md, ok := metadata.FromIncomingContext(ctx); ok {
				keys := make([]string, 0, len(md))
				for k := range md {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				var parts []string
				for _, k := range keys {
					v := strings.Join(md[k], ",")
					if redacted[k] {
						v = "[REDACTED]"
					}
					parts = append(parts, k+"="+v)
				}
				logger.Log(ctx, LevelTrace, service+"."+method+" metadata: "+strings.Join(parts, " "))
			}
		}
		return resp, err
	}
}

// canonical is a code's API name, NOT_FOUND rather than gRPC's NotFound, as
// the error a client shows spells it.
func canonical(c codes.Code) string { return rpccode.Code(c).String() }
