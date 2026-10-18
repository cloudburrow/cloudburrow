//go:build upgrade

// Package upgrade checks that state an older CloudBurrow release wrote
// still loads in this one (#1605): a persistent instance's state directory,
// and a `state save` archive. See docs/install.md, "Upgrading".
//
// Every client here is built from an endpoint the instance's `cloudburrow
// env` printed, and refuses to start without one, so none can fall back
// to Google's.
package upgrade

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// env is what `cloudburrow env` exported, parsed. It is never printed: it
// can hold the admin token and database passwords.
type env map[string]string

// readEnv runs `cloudburrow env` with args and parses its exports.
func readEnv(t *testing.T, bin string, args ...string) env {
	t.Helper()
	out, err := exec.Command(bin, append([]string{"env"}, args...)...).Output()
	if err != nil {
		t.Fatalf("%s env: %v", bin, err)
	}
	e := env{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "export ")
		if !ok {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if i := strings.Index(v, "  #"); i >= 0 {
			v = v[:i]
		}
		e[k] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return e
}

// need returns the variable, or fails: an empty endpoint must never let a
// client choose Google's default.
func (e env) need(t *testing.T, name string) string {
	t.Helper()
	v := e[name]
	if v == "" {
		t.Fatalf("cloudburrow env exported no %s", name)
	}
	return v
}

// grpcOpts are the options of a gRPC client of a plaintext local endpoint.
func grpcOpts(t *testing.T, addr string) []option.ClientOption {
	t.Helper()
	if addr == "" {
		t.Fatal("empty endpoint")
	}
	return []option.ClientOption{
		option.WithEndpoint(addr),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	}
}

// httpOpts are the options of a REST client of a local endpoint.
func httpOpts(t *testing.T, base string) []option.ClientOption {
	t.Helper()
	if base == "" {
		t.Fatal("empty endpoint")
	}
	return []option.ClientOption{option.WithEndpoint(base), option.WithoutAuthentication()}
}

// result is one service's outcome, for the report the test logs.
type result struct {
	service string
	err     error
}

func (r result) String() string {
	if r.err == nil {
		return r.service + ": ok"
	}
	return fmt.Sprintf("%s: %v", r.service, r.err)
}

func check(ctx context.Context, results *[]result, service string, f func(ctx context.Context) error) {
	*results = append(*results, result{service, f(ctx)})
}
