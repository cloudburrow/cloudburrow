package storagenotify

import (
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Handler serves the Cloud Storage notificationConfigs API and forwards
// everything else to the storage backend.
//
// It sits in front of the backend rather than beside it because the official
// clients send every Storage call to one endpoint. Publishing
// notificationConfigs on a second port would mean a client could reach the
// rest of the API and not this, which is a shape no Google endpoint has.
type Handler struct {
	configs *Store
	proxy   *httputil.ReverseProxy
}

// NewHandler returns a handler forwarding unmatched requests to backend.
func NewHandler(configs *Store, backend *url.URL) *Handler {
	proxy := httputil.NewSingleHostReverseProxy(backend)
	// The backend matches its virtual-host download path against the Host it
	// was configured with, so the original Host must survive the hop.
	// Rewriting it to the backend's address would 404 every object read.
	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		host := r.Host
		director(r)
		r.Host = host
	}
	return &Handler{configs: configs, proxy: proxy}
}

// notificationConfigsPath matches the two shapes the API defines.
//
//	/storage/v1/b/{bucket}/notificationConfigs
//	/storage/v1/b/{bucket}/notificationConfigs/{id}
//
// Google also serves the older `/notificationConfigs` spelling as
// `/notificationConfigs`; both are accepted because the Go client uses the
// first and older clients use the second.
func notificationConfigsPath(path string) (bucket, id string, ok bool) {
	for _, collection := range []string{"/notificationConfigs", "/notificationconfigs"} {
		prefix := "/storage/v1/b/"
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(path, prefix)
		idx := strings.Index(rest, collection)
		if idx < 0 {
			continue
		}
		bucket = rest[:idx]
		tail := strings.TrimPrefix(rest[idx+len(collection):], "/")
		if bucket == "" || strings.Contains(bucket, "/") {
			continue
		}
		if strings.Contains(tail, "/") {
			continue
		}
		return bucket, tail, true
	}
	return "", "", false
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bucket, id, ok := notificationConfigsPath(r.URL.Path)
	if !ok {
		// Everything that is not a notification call belongs to the backend.
		h.proxy.ServeHTTP(w, r)
		return
	}

	var err error
	switch {
	case r.Method == http.MethodPost && id == "":
		err = h.create(w, r, bucket)
	case r.Method == http.MethodGet && id == "":
		err = h.list(w, bucket)
	case r.Method == http.MethodGet:
		err = h.get(w, bucket, id)
	case r.Method == http.MethodDelete && id != "":
		err = h.delete(w, bucket, id)
	default:
		err = apierror.InvalidArgument("%s is not supported on %s", r.Method, r.URL.Path)
	}
	if err != nil {
		apierror.WriteJSON(w, err)
	}
}

// jsonConfig is the wire representation.
type jsonConfig struct {
	Kind             string            `json:"kind"`
	ID               string            `json:"id"`
	SelfLink         string            `json:"selfLink"`
	Topic            string            `json:"topic"`
	EventTypes       []string          `json:"event_types,omitempty"`
	CustomAttributes map[string]string `json:"custom_attributes,omitempty"`
	ObjectNamePrefix string            `json:"object_name_prefix,omitempty"`
	PayloadFormat    string            `json:"payload_format"`
	Etag             string            `json:"etag,omitempty"`
}

func render(c Config) jsonConfig {
	types := make([]string, 0, len(c.EventTypes))
	for _, t := range c.EventTypes {
		types = append(types, string(t))
	}
	return jsonConfig{
		Kind:     "storage#notification",
		ID:       c.ID,
		SelfLink: c.SelfLink(),
		// Reported in the service-qualified form, because that is what the
		// official client sends and what it parses back.
		Topic:            QualifiedTopic(c.Topic),
		EventTypes:       types,
		CustomAttributes: c.CustomAttributes,
		ObjectNamePrefix: c.ObjectNamePrefix,
		PayloadFormat:    string(c.PayloadFormat),
		Etag:             c.Etag,
	}
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request, bucket string) error {
	var body jsonConfig
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return apierror.InvalidArgument("malformed notification configuration: %v", err)
	}

	types := make([]EventType, 0, len(body.EventTypes))
	for _, t := range body.EventTypes {
		types = append(types, EventType(t))
	}
	created, err := h.configs.Create(bucket, Config{
		Topic:            body.Topic,
		EventTypes:       types,
		ObjectNamePrefix: body.ObjectNamePrefix,
		CustomAttributes: body.CustomAttributes,
		PayloadFormat:    PayloadFormat(body.PayloadFormat),
	})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, render(created))
}

func (h *Handler) get(w http.ResponseWriter, bucket, id string) error {
	c, err := h.configs.Get(bucket, id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, render(c))
}

func (h *Handler) list(w http.ResponseWriter, bucket string) error {
	all, err := h.configs.List(bucket)
	if err != nil {
		return err
	}
	items := make([]jsonConfig, 0, len(all))
	for _, c := range all {
		items = append(items, render(c))
	}
	return writeJSON(w, http.StatusOK, map[string]any{
		"kind":  "storage#notifications",
		"items": items,
	})
}

func (h *Handler) delete(w http.ResponseWriter, bucket, id string) error {
	if err := h.configs.Delete(bucket, id); err != nil {
		return err
	}
	// Cloud Storage returns 204 with no body for this call.
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(body)
}

// ProxyTimeouts are the bounds the fronting server applies. They are generous
// because an upload of a large object legitimately takes time, and a timeout
// that cut one short would look like data corruption.
const (
	ProxyReadHeaderTimeout = 30 * time.Second
	ProxyIdleTimeout       = 120 * time.Second
)
