package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// RESTServer serves the Secret Manager v1 JSON API.
//
// Google's REST surface is a transcoding of the same proto service, so this
// speaks to the same store as the gRPC server rather than reimplementing any
// behaviour — two implementations of one contract would drift.
type RESTServer struct {
	store *Store
}

// NewRESTServer returns the JSON surface.
func NewRESTServer(s *Store) *RESTServer { return &RESTServer{store: s} }

// Routes registers the v1 paths.
//
// Google's JSON API appends a custom method to the last path segment
// (`.../secrets/my-key:addVersion`). net/http's pattern matcher cannot
// express a wildcard followed by a literal in one segment, so the verb is
// captured as part of the value and split off by the handler. Splitting here
// rather than working around it with a catch-all keeps every route explicit.
func (h *RESTServer) Routes(r *rest.Router) {
	r.Handle("POST /v1/projects/{project}/secrets", h.createSecret)
	r.Handle("GET /v1/projects/{project}/secrets", h.listSecrets)
	r.Handle("GET /v1/projects/{project}/secrets/{secret}", h.getSecret)
	r.Handle("DELETE /v1/projects/{project}/secrets/{secret}", h.deleteSecret)
	// POST on a secret is always a custom method; there is no plain create
	// at this path.
	r.Handle("POST /v1/projects/{project}/secrets/{secret}", h.secretVerb)

	r.Handle("GET /v1/projects/{project}/secrets/{secret}/versions", h.listVersions)
	r.Handle("GET /v1/projects/{project}/secrets/{secret}/versions/{version}", h.getOrAccessVersion)
	r.Handle("POST /v1/projects/{project}/secrets/{secret}/versions/{version}", h.versionVerb)
}

// splitVerb separates a custom method from the resource ID it was appended
// to. A value with no colon has no verb.
func splitVerb(value string) (id, verb string) {
	if i := strings.IndexByte(value, ':'); i >= 0 {
		return value[:i], value[i+1:]
	}
	return value, ""
}

// secretVerb dispatches POST .../secrets/{secret}:{verb}.
func (h *RESTServer) secretVerb(w http.ResponseWriter, r *http.Request) error {
	raw, err := rest.PathValue(r, "secret")
	if err != nil {
		return err
	}
	_, verb := splitVerb(raw)
	switch verb {
	case "addVersion":
		return h.addVersion(w, r)
	case "getIamPolicy":
		return h.getIamPolicy(w, r)
	case "setIamPolicy":
		return h.setIamPolicy(w, r)
	case "testIamPermissions":
		return h.testIamPermissions(w, r)
	case "":
		return apierror.InvalidArgument(
			"POST to a secret requires a custom method, for example %s:addVersion", raw)
	default:
		// Unimplemented rather than 404: the resource exists and the method
		// is one Secret Manager defines; CloudBurrow simply does not serve it.
		return apierror.Unimplemented("custom method %q is not implemented", verb)
	}
}

// getOrAccessVersion dispatches GET .../versions/{version} and
// .../versions/{version}:access.
func (h *RESTServer) getOrAccessVersion(w http.ResponseWriter, r *http.Request) error {
	raw, err := rest.PathValue(r, "version")
	if err != nil {
		return err
	}
	_, verb := splitVerb(raw)
	switch verb {
	case "":
		return h.getVersion(w, r)
	case "access":
		return h.accessVersion(w, r)
	default:
		return apierror.Unimplemented("custom method %q is not implemented", verb)
	}
}

// versionVerb dispatches POST .../versions/{version}:{verb}.
func (h *RESTServer) versionVerb(w http.ResponseWriter, r *http.Request) error {
	raw, err := rest.PathValue(r, "version")
	if err != nil {
		return err
	}
	_, verb := splitVerb(raw)
	switch verb {
	case "enable":
		return h.changeState(w, r, StateEnabled)
	case "disable":
		return h.changeState(w, r, StateDisabled)
	case "destroy":
		return h.destroyVersion(w, r)
	case "":
		return apierror.InvalidArgument(
			"POST to a secret version requires a custom method, for example %s:destroy", raw)
	default:
		return apierror.Unimplemented("custom method %q is not implemented", verb)
	}
}

// jsonSecret is the REST representation. It is written by hand rather than
// marshalled from the proto because the JSON API uses its own casing and
// omits fields the proto always carries.
type jsonSecret struct {
	Name        string            `json:"name"`
	Replication map[string]any    `json:"replication"`
	CreateTime  string            `json:"createTime"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Etag        string            `json:"etag,omitempty"`
}

type jsonVersion struct {
	Name        string `json:"name"`
	CreateTime  string `json:"createTime"`
	DestroyTime string `json:"destroyTime,omitempty"`
	State       string `json:"state"`
	Etag        string `json:"etag,omitempty"`
}

func renderSecret(s Secret) jsonSecret {
	repl := map[string]any{"automatic": map[string]any{}}
	if s.Replication == "user-managed" {
		repl = map[string]any{"userManaged": map[string]any{}}
	}
	return jsonSecret{
		Name:        s.Name,
		Replication: repl,
		CreateTime:  s.Created.UTC().Format(time.RFC3339Nano),
		Labels:      s.Labels,
		Annotations: s.Annotations,
		Etag:        s.Etag,
	}
}

func renderVersion(v Version) jsonVersion {
	out := jsonVersion{
		Name:       v.Name,
		CreateTime: v.Created.UTC().Format(time.RFC3339Nano),
		State:      string(v.State),
		Etag:       v.Etag,
	}
	if !v.Destroyed.IsZero() {
		out.DestroyTime = v.Destroyed.UTC().Format(time.RFC3339Nano)
	}
	return out
}

func (h *RESTServer) parts(r *http.Request) (project, secret string, err error) {
	project, err = rest.PathValue(r, "project")
	if err != nil {
		return "", "", err
	}
	secret, err = rest.PathValue(r, "secret")
	if err != nil {
		return "", "", err
	}
	secret, _ = splitVerb(secret)
	if err := ValidateSecretID(secret); err != nil {
		return "", "", err
	}
	return project, secret, nil
}

func (h *RESTServer) createSecret(w http.ResponseWriter, r *http.Request) error {
	project, err := rest.PathValue(r, "project")
	if err != nil {
		return err
	}
	id := r.URL.Query().Get("secretId")
	if id == "" {
		return apierror.InvalidArgument("secretId query parameter is required")
	}

	var body struct {
		Replication map[string]any    `json:"replication"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	}
	// An absent body is legitimate: replication defaults to automatic.
	if r.ContentLength > 0 {
		if err := rest.DecodeJSON(r, &body); err != nil {
			return err
		}
	}
	replication := "automatic"
	if _, ok := body.Replication["userManaged"]; ok {
		replication = "user-managed"
	}

	sec, err := h.store.CreateSecret(project, id, body.Labels, body.Annotations, replication)
	if err != nil {
		return err
	}
	return rest.WriteJSON(w, http.StatusOK, renderSecret(sec))
}

func (h *RESTServer) getSecret(w http.ResponseWriter, r *http.Request) error {
	raw, err := rest.PathValue(r, "secret")
	if err != nil {
		return err
	}
	// GET .../secrets/{secret}:getIamPolicy is a custom method; any other
	// verb is one this does not serve, never the secret itself.
	switch _, verb := splitVerb(raw); verb {
	case "":
	case "getIamPolicy":
		return h.getIamPolicy(w, r)
	default:
		return apierror.Unimplemented("custom method %q is not implemented", verb)
	}
	project, secret, err := h.parts(r)
	if err != nil {
		return err
	}
	sec, err := h.store.GetSecret(project, secret)
	if err != nil {
		return err
	}
	return rest.WriteJSON(w, http.StatusOK, renderSecret(sec))
}

func (h *RESTServer) deleteSecret(w http.ResponseWriter, r *http.Request) error {
	project, secret, err := h.parts(r)
	if err != nil {
		return err
	}
	if err := h.store.DeleteSecret(project, secret); err != nil {
		return err
	}
	return rest.WriteJSON(w, http.StatusOK, map[string]any{})
}

func (h *RESTServer) listSecrets(w http.ResponseWriter, r *http.Request) error {
	project, err := rest.PathValue(r, "project")
	if err != nil {
		return err
	}
	all, err := h.store.ListSecrets(project)
	if err != nil {
		return err
	}
	out := make([]jsonSecret, 0, len(all))
	for _, s := range all {
		out = append(out, renderSecret(s))
	}
	return rest.WriteJSON(w, http.StatusOK, map[string]any{
		"secrets": out, "totalSize": len(all),
	})
}

func (h *RESTServer) addVersion(w http.ResponseWriter, r *http.Request) error {
	project, secret, err := h.parts(r)
	if err != nil {
		return err
	}
	var body struct {
		Payload struct {
			Data string `json:"data"`
		} `json:"payload"`
	}
	if err := rest.DecodeJSON(r, &body); err != nil {
		return err
	}
	// The JSON API carries bytes as base64, so a client sending raw text
	// would otherwise store the text of its own encoding mistake.
	data, decodeErr := base64.StdEncoding.DecodeString(body.Payload.Data)
	if decodeErr != nil {
		return apierror.InvalidArgument("payload.data must be base64: %v", decodeErr)
	}

	v, err := h.store.AddVersion(project, secret, data)
	if err != nil {
		return err
	}
	return rest.WriteJSON(w, http.StatusOK, renderVersion(v))
}

func (h *RESTServer) versionRef(r *http.Request) (project, secret, version string, err error) {
	project, secret, err = h.parts(r)
	if err != nil {
		return "", "", "", err
	}
	version, err = rest.PathValue(r, "version")
	if err != nil {
		return "", "", "", err
	}
	version, _ = splitVerb(version)
	// Reuse the same validation the gRPC surface applies, so the two agree
	// on what a version reference is.
	if _, _, _, err := ParseVersionName(
		fmt.Sprintf("projects/%s/secrets/%s/versions/%s", project, secret, version)); err != nil {
		return "", "", "", err
	}
	return project, secret, version, nil
}

func (h *RESTServer) getVersion(w http.ResponseWriter, r *http.Request) error {
	project, secret, version, err := h.versionRef(r)
	if err != nil {
		return err
	}
	v, err := h.store.GetVersion(project, secret, version)
	if err != nil {
		return err
	}
	return rest.WriteJSON(w, http.StatusOK, renderVersion(v))
}

func (h *RESTServer) accessVersion(w http.ResponseWriter, r *http.Request) error {
	project, secret, version, err := h.versionRef(r)
	if err != nil {
		return err
	}
	v, err := h.store.AccessVersion(project, secret, version)
	if err != nil {
		return err
	}
	return rest.WriteJSON(w, http.StatusOK, map[string]any{
		"name": v.Name,
		"payload": map[string]string{
			"data": base64.StdEncoding.EncodeToString(v.Payload),
		},
	})
}

func (h *RESTServer) listVersions(w http.ResponseWriter, r *http.Request) error {
	project, secret, err := h.parts(r)
	if err != nil {
		return err
	}
	all, err := h.store.ListVersions(project, secret)
	if err != nil {
		return err
	}
	out := make([]jsonVersion, 0, len(all))
	for _, v := range all {
		out = append(out, renderVersion(v))
	}
	return rest.WriteJSON(w, http.StatusOK, map[string]any{
		"versions": out, "totalSize": len(all),
	})
}

func (h *RESTServer) changeState(w http.ResponseWriter, r *http.Request, state VersionState) error {
	project, secret, version, err := h.versionRef(r)
	if err != nil {
		return err
	}
	v, err := h.store.SetVersionState(project, secret, version, state)
	if err != nil {
		return err
	}
	return rest.WriteJSON(w, http.StatusOK, renderVersion(v))
}

func (h *RESTServer) destroyVersion(w http.ResponseWriter, r *http.Request) error {
	project, secret, version, err := h.versionRef(r)
	if err != nil {
		return err
	}
	v, err := h.store.DestroyVersion(project, secret, version)
	if err != nil {
		return err
	}
	return rest.WriteJSON(w, http.StatusOK, renderVersion(v))
}

// --- IAM policies (ADR-0006, #365): stored, never enforced ---------------

// decodeProto reads an optional JSON body into a request message. Unknown
// fields are refused, as DecodeJSON refuses them.
func decodeProto(r *http.Request, m proto.Message) error {
	if r.Body == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, rest.MaxRequestBytes+1))
	if err != nil {
		return apierror.InvalidArgument("read request body: %v", err)
	}
	if len(b) > rest.MaxRequestBytes {
		return apierror.InvalidArgument("request body exceeds %d bytes", rest.MaxRequestBytes)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := protojson.Unmarshal(b, m); err != nil {
		return apierror.InvalidArgument("malformed request body: %v", err)
	}
	return nil
}

func writeProto(w http.ResponseWriter, m proto.Message) error {
	b, err := protojson.Marshal(m)
	if err != nil {
		return apierror.Internal(err, "encode response")
	}
	return rest.WriteJSON(w, http.StatusOK, json.RawMessage(b))
}

// getIamPolicy serves GET and POST .../secrets/{secret}:getIamPolicy. GET
// carries the options in the query, POST in the body.
func (h *RESTServer) getIamPolicy(w http.ResponseWriter, r *http.Request) error {
	project, secret, err := h.parts(r)
	if err != nil {
		return err
	}
	req := &iampb.GetIamPolicyRequest{}
	if r.Method == http.MethodPost {
		if err := decodeProto(r, req); err != nil {
			return err
		}
	} else if v := r.URL.Query().Get("options.requestedPolicyVersion"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return apierror.InvalidArgument("options.requestedPolicyVersion %q is not a number", v)
		}
		req.Options = &iampb.GetPolicyOptions{RequestedPolicyVersion: int32(n)}
	}
	return h.writePolicy(w, func() (*iampb.Policy, error) {
		req.Resource = SecretName(project, secret)
		return NewGRPCServer(h.store).GetIamPolicy(r.Context(), req)
	})
}

func (h *RESTServer) setIamPolicy(w http.ResponseWriter, r *http.Request) error {
	project, secret, err := h.parts(r)
	if err != nil {
		return err
	}
	req := &iampb.SetIamPolicyRequest{}
	if err := decodeProto(r, req); err != nil {
		return err
	}
	return h.writePolicy(w, func() (*iampb.Policy, error) {
		req.Resource = SecretName(project, secret)
		return NewGRPCServer(h.store).SetIamPolicy(r.Context(), req)
	})
}

func (h *RESTServer) writePolicy(w http.ResponseWriter, call func() (*iampb.Policy, error)) error {
	p, err := call()
	if err != nil {
		return err
	}
	return writeProto(w, p)
}

func (h *RESTServer) testIamPermissions(w http.ResponseWriter, r *http.Request) error {
	project, secret, err := h.parts(r)
	if err != nil {
		return err
	}
	req := &iampb.TestIamPermissionsRequest{}
	if err := decodeProto(r, req); err != nil {
		return err
	}
	req.Resource = SecretName(project, secret)
	resp, err := NewGRPCServer(h.store).TestIamPermissions(r.Context(), req)
	if err != nil {
		return err
	}
	return writeProto(w, resp)
}
