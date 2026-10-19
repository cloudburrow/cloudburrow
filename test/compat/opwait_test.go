//go:build compat

package compat

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/googleapis/gax-go/v2"
)

// opPollEvery is how often waitLRO asks for an operation.
const opPollEvery = 500 * time.Millisecond

// pollOp is a client library's long-running operation whose Poll answers
// its result: CreateInstanceOperation, UpdateServiceOperation and the like.
type pollOp[T any] interface {
	Poll(context.Context, ...gax.CallOption) (T, error)
	Done() bool
	Name() string
}

// pollDoneOp is one whose Poll answers only an error: the Delete*Operation
// types, whose result is Empty.
type pollDoneOp interface {
	Poll(context.Context, ...gax.CallOption) error
	Done() bool
	Name() string
}

// waitLRO polls op every half second until it is done or ctx ends, and
// answers what op.Wait would: its result, or its error unchanged (#1667).
// op.Wait backs off between polls (gax: 1 s doubling, capped at a minute,
// each pause drawn from (0, cur]), so under h.Context()'s minute a creation
// that takes more than about 12 s can be missed: the next poll lands past
// the deadline (#1665). When ctx ends first the error names the operation
// and how long it was waited for. Pass it a context sized for the
// operation, such as memorystoreWait(t) or runWait(t), not h.Context().
func waitLRO[T any](ctx context.Context, op pollOp[T]) (T, error) {
	start := time.Now()
	for {
		out, err := op.Poll(ctx)
		if ctx.Err() != nil && (err != nil || !op.Done()) {
			return out, fmt.Errorf("operation %s not done after %v: %w", op.Name(), time.Since(start).Round(100*time.Millisecond), ctx.Err())
		}
		if err != nil || op.Done() {
			return out, err
		}
		select {
		case <-ctx.Done():
		case <-time.After(opPollEvery):
		}
	}
}

// waitLRODone is waitLRO for an operation whose Poll answers only an error.
func waitLRODone(ctx context.Context, op pollDoneOp) error {
	_, err := waitLRO[struct{}](ctx, doneOp{op})
	return err
}

type doneOp struct{ pollDoneOp }

func (d doneOp) Poll(ctx context.Context, opts ...gax.CallOption) (struct{}, error) {
	return struct{}{}, d.pollDoneOp.Poll(ctx, opts...)
}

// runWait is a context for waiting on one Cloud Run operation: a service's
// or worker pool's revision becoming ready, or a job's execution, starts
// containers on the cluster and can outlast h.Context()'s minute on a
// loaded machine.
func runWait(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}
