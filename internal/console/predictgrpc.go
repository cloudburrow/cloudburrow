package console

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/prediction"
)

// DirectRawPredict on the Online prediction screen (#1477).
//
// A model that declares grpcPorts — a Cloud Run service whose port is named
// h2c — serves gRPC, and Vertex AI's DirectRawPredict calls method_name on it
// with raw request bytes. The screen offers the same call on such an
// endpoint: a method name and the input bytes, base64, sent through the
// cluster ingress to the container's gRPC server exactly as the local AI
// endpoint's DirectRawPredict sends them, and the output bytes or the
// container's status shown as they came back.

// directRawRequest is what the screen sends.
type directRawRequest struct {
	Endpoint   string `json:"endpoint"`
	MethodName string `json:"methodName"`
	// Input is the request message's bytes, base64; empty is the empty
	// message.
	Input string `json:"input"`
}

// DirectRawResult is the container's answer.
type DirectRawResult struct {
	Endpoint string `json:"endpoint"`
	Method   string `json:"method"`
	// Authority is the host the call was routed on.
	Authority string `json:"authority"`
	// Code is the gRPC status code name, OK on success.
	Code string `json:"code"`
	// Message is the container's status message when it refused.
	Message string `json:"message,omitempty"`
	// Output is the response message's bytes, base64, and OutputHex the
	// same bytes in hex.
	Output     string `json:"output"`
	OutputHex  string `json:"outputHex"`
	InputBytes int    `json:"inputBytes"`
	DurationMs int64  `json:"durationMs"`
}

// directRawHexLimit bounds the hex rendering, which is twice the bytes.
const directRawHexLimit = 64 << 10

func (s *Server) handleDirectRawPredict(w http.ResponseWriter, r *http.Request) {
	p := s.prediction
	if !p.Configured() {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "online prediction needs Cloud Run, which is not enabled on this instance",
		})
		return
	}
	var req directRawRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 2*predictBodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed request: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Endpoint) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "choose an endpoint"})
		return
	}
	method, err := prediction.GRPCMethod(strings.TrimSpace(req.MethodName))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	input, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(req.Input), ""))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the input is not base64: " + err.Error()})
		return
	}
	if len(input) > predictBodyLimit {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("the input is %d bytes; the console sends at most %d", len(input), predictBodyLimit),
		})
		return
	}

	ep, gateway, ok := p.lookup(w, r, req.Endpoint)
	if !ok {
		return
	}
	if ep.GRPCPort == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("endpoint %s serves no gRPC: its model "+
			"declares no grpcPorts (a Cloud Run container port named h2c)", ep.ID)})
		return
	}
	u, err := url.Parse(ep.URI)
	if err != nil || u.Host == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("endpoint %s is %s and has no URL yet", ep.ID, ep.State)})
		return
	}
	conn, err := prediction.DialModel(gateway, u.Host)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(r.Context(), predictDeadline)
	defer cancel()
	started := time.Now()
	var out []byte
	err = conn.Invoke(ctx, method, &input, &out)
	result := DirectRawResult{
		Endpoint: ep.Name, Method: method, Authority: u.Host, Code: "OK",
		InputBytes: len(input), DurationMs: time.Since(started).Milliseconds(),
	}
	if err != nil {
		// The container's own status, or the transport's, as it came back.
		st := status.Convert(err)
		result.Code, result.Message = st.Code().String(), st.Message()
	}
	result.Output = base64.StdEncoding.EncodeToString(out)
	h := out
	if len(h) > directRawHexLimit {
		h = h[:directRawHexLimit]
	}
	result.OutputHex = hex.EncodeToString(h)
	writeJSON(w, http.StatusOK, result)
}
