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

// RESTServer serves the Cloud Tasks v2 JSON API: queues and their IAM
// policies by hand, as Terraform and other REST clients use them (#366), and
// tasks and UpdateQueue through the shared transcoder (#591, #692).
//
// Both are transcodings of the gRPC service, not a second implementation:
// every queue handler decodes the JSON body into the request message and
// calls the same GRPCServer method, and a task path or a queue PATCH goes to
// the transcoder, with that GRPCServer registered, by Google's own
// google.api.http bindings.
// Every method the gRPC service leaves unimplemented (RunTask) stays
// UNIMPLEMENTED here.
//
// The queue routes predate the transcoder and keep their behaviour: GET
// :getIamPolicy as well as POST, and unknown query parameters ignored. Task
// paths and PATCH follow the transcoder's policy, under which an unknown
// query parameter is INVALID_ARGUMENT.
type RESTServer struct {
	g     *GRPCServer
	tasks *rest.Transcoder
}

// NewRESTServer returns the JSON API over the same store.
func NewRESTServer(s *Store) *RESTServer { return NewRESTServerFor(NewGRPCServer(s)) }

// NewRESTServerFor returns the JSON API for g, the server gRPC registers, so
// both transports call the same one.
func NewRESTServerFor(g *GRPCServer) *RESTServer {
	t := &rest.Transcoder{Packages: []string{"google.cloud.tasks.v2"}}
	g.Register(t)
	return &RESTServer{g: g, tasks: t}
}

// Routes registers the v2 queue paths.
func (h *RESTServer) Routes(r *rest.Router) {
	const loc = "/v2/projects/{project}/locations/{location}"
	r.Handle("POST "+loc+"/queues", h.createQueue)
	r.Handle("GET "+loc+"/queues", h.listQueues)
	r.Handle("GET "+loc+"/queues/{queue}", h.getQueue)
	r.Handle("DELETE "+loc+"/queues/{queue}", h.deleteQueue)
	// UpdateQueue is transcoded (#692), so updateMask is Google's
	// FieldMask in lowerCamelCase, converted to the proto paths the gRPC
	// method checks, and an unknown query parameter is INVALID_ARGUMENT.
	r.Handle("PATCH "+loc+"/queues/{queue}", h.transcode)
	// POST on a queue is always a custom method: pause, resume, purge and
	// the IAM methods.
	r.Handle("POST "+loc+"/queues/{queue}", h.queueVerb)
	// Tasks, every method, go to the transcoder, which matches Google's
	// bindings: CreateTask and ListTasks on .../tasks; GetTask, DeleteTask
	// and :run on .../tasks/{task}. It sees no queue path.
	r.Handle(loc+"/queues/{queue}/tasks", h.transcode)
	r.Handle(loc+"/queues/{queue}/tasks/{task}", h.transcode)
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

func (h *RESTServer) transcode(w http.ResponseWriter, r *http.Request) error {
	h.tasks.ServeHTTP(w, r)
	return nil
}
