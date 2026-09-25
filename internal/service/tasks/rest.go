package tasks

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// RESTServer serves the part of the Cloud Tasks v2 JSON API that Terraform
// and other REST clients use for queues and their IAM policies (#366).
//
// It is a transcoding of the gRPC service, not a second implementation:
// every handler decodes the JSON body into the request message and calls the
// same GRPCServer method. Tasks over JSON are not routed, and every other
// queue method the gRPC service leaves unimplemented stays UNIMPLEMENTED
// here.
type RESTServer struct{ g *GRPCServer }

// NewRESTServer returns the JSON API over the same store.
func NewRESTServer(s *Store) *RESTServer { return &RESTServer{g: NewGRPCServer(s)} }

// Routes registers the v2 queue paths.
func (h *RESTServer) Routes(r *rest.Router) {
	const loc = "/v2/projects/{project}/locations/{location}"
	r.Handle("POST "+loc+"/queues", h.createQueue)
	r.Handle("GET "+loc+"/queues", h.listQueues)
	r.Handle("GET "+loc+"/queues/{queue}", h.getQueue)
	r.Handle("DELETE "+loc+"/queues/{queue}", h.deleteQueue)
	r.Handle("PATCH "+loc+"/queues/{queue}", h.patchQueue)
	// POST on a queue is always a custom method: pause, resume, purge and
	// the IAM methods.
	r.Handle("POST "+loc+"/queues/{queue}", h.queueVerb)
	r.Handle(loc+"/queues/{queue}/tasks", h.tasksNotServed)
	r.Handle(loc+"/queues/{queue}/tasks/{task}", h.tasksNotServed)
}

func (h *RESTServer) parent(r *http.Request) string {
	return "projects/" + r.PathValue("project") + "/locations/" + r.PathValue("location")
}

// queue returns the queue's name and the custom method appended to it.
func (h *RESTServer) queue(r *http.Request) (name, verb string) {
	id := r.PathValue("queue")
	if i := strings.IndexByte(id, ':'); i >= 0 {
		id, verb = id[:i], id[i+1:]
	}
	return h.parent(r) + "/queues/" + id, verb
}

// decode reads an optional JSON body into a request message. Unknown
// fields are refused: a client sending a field that is dropped would
// believe it took effect.
func decode(r *http.Request, m proto.Message) error {
	if r.Body == nil {
		return nil
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return apierror.InvalidArgument("read request body: %v", err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := protojson.Unmarshal(b, m); err != nil {
		return apierror.InvalidArgument("malformed request body: %v", err)
	}
	return nil
}

func write(w http.ResponseWriter, m proto.Message, err error) error {
	if err != nil {
		return err
	}
	b, err := protojson.Marshal(m)
	if err != nil {
		return apierror.Internal(err, "encode response")
	}
	return rest.WriteJSON(w, http.StatusOK, json.RawMessage(b))
}

func (h *RESTServer) createQueue(w http.ResponseWriter, r *http.Request) error {
	q := &taskspb.Queue{}
	if err := decode(r, q); err != nil {
		return err
	}
	resp, err := h.g.CreateQueue(r.Context(), &taskspb.CreateQueueRequest{Parent: h.parent(r), Queue: q})
	return write(w, resp, err)
}

func (h *RESTServer) listQueues(w http.ResponseWriter, r *http.Request) error {
	size, err := rest.QueryInt(r, "pageSize", 0)
	if err != nil {
		return err
	}
	resp, err := h.g.ListQueues(r.Context(), &taskspb.ListQueuesRequest{Parent: h.parent(r),
		PageSize: int32(size), PageToken: r.URL.Query().Get("pageToken"), Filter: r.URL.Query().Get("filter")})
	return write(w, resp, err)
}

func (h *RESTServer) getQueue(w http.ResponseWriter, r *http.Request) error {
	name, verb := h.queue(r)
	switch verb {
	case "":
	case "getIamPolicy":
		return h.getIamPolicy(w, r, name)
	default:
		return apierror.Unimplemented("custom method %q is not implemented", verb)
	}
	resp, err := h.g.GetQueue(r.Context(), &taskspb.GetQueueRequest{Name: name})
	return write(w, resp, err)
}

func (h *RESTServer) deleteQueue(w http.ResponseWriter, r *http.Request) error {
	name, _ := h.queue(r)
	resp, err := h.g.DeleteQueue(r.Context(), &taskspb.DeleteQueueRequest{Name: name})
	return write(w, resp, err)
}

// patchQueue is UpdateQueue, which the gRPC service does not implement, so
// it answers UNIMPLEMENTED too rather than half-applying a mask.
func (h *RESTServer) patchQueue(w http.ResponseWriter, r *http.Request) error {
	name, _ := h.queue(r)
	q := &taskspb.Queue{}
	if err := decode(r, q); err != nil {
		return err
	}
	q.Name = name
	resp, err := h.g.UpdateQueue(r.Context(), &taskspb.UpdateQueueRequest{Queue: q})
	return write(w, resp, err)
}

func (h *RESTServer) queueVerb(w http.ResponseWriter, r *http.Request) error {
	name, verb := h.queue(r)
	ctx := r.Context()
	switch verb {
	case "pause":
		resp, err := h.g.PauseQueue(ctx, &taskspb.PauseQueueRequest{Name: name})
		return write(w, resp, err)
	case "resume":
		resp, err := h.g.ResumeQueue(ctx, &taskspb.ResumeQueueRequest{Name: name})
		return write(w, resp, err)
	case "purge":
		resp, err := h.g.PurgeQueue(ctx, &taskspb.PurgeQueueRequest{Name: name})
		return write(w, resp, err)
	case "getIamPolicy":
		return h.getIamPolicy(w, r, name)
	case "setIamPolicy":
		req := &iampb.SetIamPolicyRequest{}
		if err := decode(r, req); err != nil {
			return err
		}
		req.Resource = name
		resp, err := h.g.SetIamPolicy(ctx, req)
		return write(w, resp, err)
	case "testIamPermissions":
		req := &iampb.TestIamPermissionsRequest{}
		if err := decode(r, req); err != nil {
			return err
		}
		req.Resource = name
		resp, err := h.g.TestIamPermissions(ctx, req)
		return write(w, resp, err)
	case "":
		return apierror.InvalidArgument("POST to a queue requires a custom method, for example %s:pause", name)
	default:
		return apierror.Unimplemented("custom method %q is not implemented", verb)
	}
}

// getIamPolicy serves GET (options in the query) and POST (options in the
// body) :getIamPolicy.
func (h *RESTServer) getIamPolicy(w http.ResponseWriter, r *http.Request, name string) error {
	req := &iampb.GetIamPolicyRequest{}
	if r.Method == http.MethodPost {
		if err := decode(r, req); err != nil {
			return err
		}
	} else if v := r.URL.Query().Get("options.requestedPolicyVersion"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return apierror.InvalidArgument("options.requestedPolicyVersion %q is not a number", v)
		}
		req.Options = &iampb.GetPolicyOptions{RequestedPolicyVersion: int32(n)}
	}
	req.Resource = name
	resp, err := h.g.GetIamPolicy(r.Context(), req)
	return write(w, resp, err)
}

func (h *RESTServer) tasksNotServed(http.ResponseWriter, *http.Request) error {
	return apierror.Unimplemented("the Cloud Tasks JSON API serves queues and their IAM policies; use the gRPC API for tasks")
}
