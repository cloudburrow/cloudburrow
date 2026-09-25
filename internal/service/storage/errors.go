package storage

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// httpError is a JSON API error with the status and reason Cloud Storage
// uses (docs.cloud.google.com/storage/docs/json_api/v1/status-codes). The
// statuses (304, 409, 412, 413) are HTTP ones with no exact gRPC code, so the
// shared apierror mapping does not fit; the envelope is the same shape.
type httpError struct {
	status  int
	reason  string
	message string
}

func (e *httpError) Error() string { return e.message }

func errorf(status int, reason, format string, a ...any) *httpError {
	return &httpError{status: status, reason: reason, message: fmt.Sprintf(format, a...)}
}

func badRequest(format string, a ...any) *httpError {
	return errorf(http.StatusBadRequest, "invalid", format, a...)
}

func required(field string) *httpError {
	return errorf(http.StatusBadRequest, "required", "Required parameter: %s", field)
}

func notFound(format string, a ...any) *httpError {
	return errorf(http.StatusNotFound, "notFound", format, a...)
}

func conflict(format string, a ...any) *httpError {
	return errorf(http.StatusConflict, "conflict", format, a...)
}

func preconditionFailed(format string, a ...any) *httpError {
	return errorf(http.StatusPreconditionFailed, "conditionNotMet", format, a...)
}

// writeError writes err as the JSON API's error envelope. A 304 carries no body.
func writeError(w http.ResponseWriter, err error) {
	e, ok := err.(*httpError)
	if !ok {
		if st, reason, isLimit := storeStatus(err); isLimit {
			e = errorf(st, reason, "%v", err)
		} else {
			e = errorf(http.StatusInternalServerError, "backendError", "internal error")
		}
	}
	if e.status == http.StatusNotModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	var body struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Errors  []struct {
				Message string `json:"message"`
				Domain  string `json:"domain"`
				Reason  string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	body.Error.Code, body.Error.Message = e.status, e.message
	body.Error.Errors = append(body.Error.Errors, struct {
		Message string `json:"message"`
		Domain  string `json:"domain"`
		Reason  string `json:"reason"`
	}{e.message, "global", e.reason})
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(e.status)
	_ = json.NewEncoder(w).Encode(body)
}
