package vertexai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// RawPredict on a publisher model (#1284).
//
// Vertex documents rawPredict on a publisher model as taking the model's own
// native request body and answering with its native response, unwrapped: no
// PredictRequest envelope around it. The two local models have one native
// body each, the same one their typed method reads:
//
//   - the embedding model's is the text-embedding :predict body
//     ({"instances": [...], "parameters": {...}}), answered as :predict
//     answers it;
//   - the generation model's is the generateContent body, answered as
//     generateContent answers it.
//
// So rawPredict calls the same function those methods call, and validates,
// refuses and answers identically. A content type other than JSON is
// refused naming http_body.content_type: neither model reads anything else.

// publisherRawPredict serves rawPredict on publisher model model. It returns
// the response body and whether it came from the embedding model (whose
// answers carry the provenance header).
func (s *Server) publisherRawPredict(ctx context.Context, model string, body []byte, contentType string) ([]byte, bool, error) {
	if err := checkRawContentType(contentType); err != nil {
		return nil, false, err
	}
	if checkEmbeddingModel(model, "rawPredict") == nil {
		resp, err := s.embedPredict(ctx, model, bytes.NewReader(body))
		if err != nil {
			return nil, true, err
		}
		out, err := json.Marshal(resp)
		if err != nil {
			return nil, true, apierror.Internal(err, "encoding the response: %v", err)
		}
		return out, true, nil
	}
	resp, err := s.generateUnary(ctx, model, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return nil, false, apierror.Internal(err, "encoding the response: %v", err)
	}
	return out, false, nil
}

// checkRawContentType accepts an absent content type or JSON.
func checkRawContentType(contentType string) error {
	if contentType == "" {
		return nil
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err == nil && mt == "application/json" {
		return nil
	}
	return apierror.InvalidArgument(
		"http_body.content_type %q is not served: the local models read only application/json "+
			"(the embedding model the :predict body, the generation model the generateContent body)", contentType)
}

// rawPredictREST serves POST .../publishers/google/models/{model}:rawPredict.
func (s *Server) rawPredictREST(w http.ResponseWriter, r *http.Request, model string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		apierror.WriteJSON(w, apierror.InvalidArgument("reading the request: %v", err))
		return
	}
	if len(body) > maxRequestBytes {
		apierror.WriteJSON(w, apierror.InvalidArgument("the request is larger than %d bytes", maxRequestBytes))
		return
	}
	out, embedded, err := s.publisherRawPredict(r.Context(), model, body, r.Header.Get("Content-Type"))
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		apierror.WriteJSON(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if embedded {
		w.Header().Set(ProvenanceHeader, provenanceValue)
	}
	_, _ = w.Write(out)
}
