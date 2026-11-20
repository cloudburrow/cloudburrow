package workflows

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// experimental.executions.run and experimental.executions.map, as
// https://docs.cloud.google.com/workflows/docs/reference/stdlib/experimental.executions/run
// and .../map document them:
//
//   - run(workflow_id, argument, location, project_id) starts an execution
//     of the workflow and waits for it; it returns the execution's result.
//     It raises ExecutionError when the execution does not succeed,
//     TimeoutError past 30 minutes, and TypeError when argument is not an
//     integer, double, boolean, string, list or map.
//   - map(workflow_id, arguments, timeout, location, project_id) starts one
//     execution for each element of arguments, all at once, and waits for
//     them all; it returns their results, in the order of arguments. It
//     raises ExecutionError when any of them does not succeed, and
//     TimeoutError past timeout seconds (default 1800).
//
// location and project_id default to the calling execution's. Google
// documents neither ExecutionError's fields nor what a missing workflow
// raises: CloudBurrow's ExecutionError carries the message "Execution
// failed or cancelled." and the execution (name, state, error), and a
// missing workflow is an HttpError 404, as the executions connector's.

// ChildRunner starts the executions experimental.executions waits for.
type ChildRunner interface {
	// StartChild starts an execution of workflow (a full resource name)
	// with argument (JSON, "" for none) and returns its name.
	StartChild(workflow, argument string) (string, error)
	// WaitChild waits for it to finish: state is its final state, result
	// its result (JSON), errPayload its error's payload.
	WaitChild(ctx context.Context, name string) (state, result, errPayload string, err error)
}

const experimentalRunTimeout = 30 * time.Minute

func (f *frame) experimental(fn string, named map[string]any) (any, error) {
	x := f.x
	if x.children == nil {
		return nil, raiseErr("SystemError", "experimental.executions is not available here")
	}
	wf, err := str(named["workflow_id"], "workflow_id")
	if err != nil {
		return nil, err
	}
	project, _ := x.env["GOOGLE_CLOUD_PROJECT_ID"].(string)
	location, _ := x.env["GOOGLE_CLOUD_LOCATION"].(string)
	if v, ok := named["project_id"]; ok && v != nil {
		if project, err = str(v, "project_id"); err != nil {
			return nil, err
		}
	}
	if v, ok := named["location"]; ok && v != nil {
		if location, err = str(v, "location"); err != nil {
			return nil, err
		}
	}
	parent := "projects/" + project + "/locations/" + location + "/workflows/" + wf
	var args []any
	timeout := experimentalRunTimeout
	switch fn {
	case "run":
		args = []any{named["argument"]}
	case "map":
		l, ok := named["arguments"].([]any)
		if !ok && named["arguments"] != nil {
			return nil, raiseErr("TypeError", "arguments must be a list, not %s", typeName(named["arguments"]))
		}
		args = l
		if t, ok := named["timeout"]; ok && t != nil {
			sec, ok := toFloat(t)
			if !ok || sec <= 0 {
				return nil, raiseErr("TypeError", "timeout must be a positive number of seconds")
			}
			timeout = time.Duration(sec * float64(time.Second))
		}
	default:
		return nil, raiseErr("KeyError", "function 'experimental.executions.%s' not found", fn)
	}
	for k := range named {
		switch k {
		case "workflow_id", "location", "project_id", "argument", "arguments", "timeout":
		default:
			return nil, raiseErr("TypeError", "experimental.executions.%s got an unexpected argument %q", fn, k)
		}
	}
	// Start them all, each start journaled so a resumed execution waits for
	// the same executions instead of starting new ones.
	names := make([]string, len(args))
	for i, a := range args {
		switch a.(type) {
		case nil, int64, float64, bool, string, []any, map[string]any:
		default:
			return nil, raiseErr("TypeError", "argument must be an integer, a double, a boolean, a string, a list or a map, not %s", typeName(a))
		}
		arg := ""
		if a != nil {
			b, err := encodeJSON(a, false)
			if err != nil {
				return nil, err
			}
			arg = string(b)
		}
		v, err := x.effect(f, "child", func() (any, error) {
			name, err := x.children.StartChild(parent, arg)
			if err != nil {
				return nil, childStartError(err)
			}
			return name, nil
		})
		if err != nil {
			return nil, err
		}
		names[i], _ = v.(string)
	}
	ctx, cancel := context.WithTimeout(x.ctx, timeout)
	defer cancel()
	results := make([]any, len(names))
	for i, name := range names {
		v, err := x.effect(f, "childwait", func() (any, error) {
			state, result, payload, err := x.children.WaitChild(ctx, name)
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, raiseErr("TimeoutError", "the execution %s did not finish in %v seconds", name, timeout.Seconds())
			}
			if err != nil {
				return nil, err
			}
			if state != "SUCCEEDED" {
				ex := map[string]any{"name": name, "state": state}
				if payload != "" {
					p, derr := decodeJSON([]byte(payload))
					if derr != nil {
						p = payload
					}
					ex["error"] = map[string]any{"payload": p}
				}
				return nil, &wfError{val: map[string]any{"message": "Execution failed or cancelled.",
					"tags": []any{"ExecutionError"}, "execution": ex}}
			}
			if result == "" {
				return nil, nil
			}
			return decodeJSON([]byte(result))
		})
		if err != nil {
			return nil, err
		}
		results[i] = v
	}
	if fn == "run" {
		return results[0], nil
	}
	return results, nil
}

// childStartError is a failed start as the workflow sees it.
func childStartError(err error) error {
	var ae *apierror.Error
	if errors.As(err, &ae) {
		code := int64(400)
		switch ae.Code {
		case codes.NotFound:
			code = 404
		case codes.PermissionDenied:
			code = 403
		case codes.FailedPrecondition:
			code = 400
		}
		return &wfError{val: map[string]any{"message": ae.Message, "code": code, "tags": []any{"HttpError"}}}
	}
	return err
}
