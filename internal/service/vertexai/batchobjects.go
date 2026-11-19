package vertexai

import (
	"context"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// ObjectWriter is the Objects that can also write, for the batch methods'
// Cloud Storage outputs (Document AI's BatchProcessDocuments, Speech-to-Text's
// BatchRecognize, #1930).
type ObjectWriter interface {
	Write(ctx context.Context, bucket, object, contentType string, data []byte) error
}

// ObjectLister is the Objects that can also list a prefix's object names,
// for Document AI's gcs_prefix input.
type ObjectLister interface {
	List(ctx context.Context, bucket, prefix string) ([]string, error)
}

// storageRW is this instance's Cloud Storage, read and written, or a
// FAILED_PRECONDITION naming what needed it.
func (s *Server) storageRW(what string) (Objects, ObjectWriter, error) {
	s.mu.RLock()
	objs := s.objects
	s.mu.RUnlock()
	w, ok := objs.(ObjectWriter)
	if objs == nil || !ok {
		return nil, nil, apierror.FailedPrecondition("%s needs Cloud Storage, which this instance does not serve", what)
	}
	return objs, w, nil
}

// splitGCS splits gs://bucket/object; object may be empty when prefix is set.
func splitGCS(uri string, prefix bool) (string, string, bool) {
	const scheme = "gs://"
	if len(uri) <= len(scheme) || uri[:len(scheme)] != scheme {
		return "", "", false
	}
	rest := uri[len(scheme):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			if i == 0 {
				return "", "", false
			}
			if rest[i+1:] == "" && !prefix {
				return "", "", false
			}
			return rest[:i], rest[i+1:], true
		}
	}
	if prefix {
		return rest, "", true
	}
	return "", "", false
}
