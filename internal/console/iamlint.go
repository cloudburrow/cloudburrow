package console

// Run Linter in the IAM condition editor (#1756), as Google's console runs
// it (docs.cloud.google.com/iam/docs/linting-policies, Console tab): the
// IAM API's iamPolicies.lintPolicy of the condition being edited, each
// result with its severity, validation unit, message and position in the
// expression, run again as the expression changes. The editor refuses Save
// while an ERROR result stands.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// LintResult is one of LintPolicy's results.
type LintResult struct {
	// Severity is the enum's name: ERROR, WARNING, NOTICE, INFO or
	// DEPRECATED.
	Severity string `json:"severity"`
	// Unit is the validation unit, such as LintValidationUnits/DateTimeCheck.
	Unit    string `json:"unit"`
	Message string `json:"message"`
	// Position is the result's offset in the expression, in characters
	// (LintResult.location_offset).
	Position int `json:"position"`
}

// PolicyLinter is the IAM API's LintPolicy, called in process on the server
// the gRPC and JSON transports serve.
type PolicyLinter interface {
	// LintCondition lints a condition's expression on resource, a full
	// resource name (or empty).
	LintCondition(ctx context.Context, resource, title, expression string) ([]LintResult, error)
}

// SetPolicyLinter attaches LintPolicy. Nil, the default, offers no Run
// Linter.
func (s *Server) SetPolicyLinter(l PolicyLinter) { s.linter = l }

// handleLintCondition is POST /api/iam/lint {resource, title, expression}.
func (s *Server) handleLintCondition(w http.ResponseWriter, r *http.Request) {
	if s.linter == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "the IAM API is not served on this instance, so it lints no conditions"})
		return
	}
	var req struct {
		Resource   string `json:"resource"`
		Title      string `json:"title"`
		Expression string `json:"expression"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed request: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Expression) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expression is required: the condition's CEL expression"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	results, err := s.linter.LintCondition(ctx, req.Resource, req.Title, req.Expression)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": userMessage(err)})
		return
	}
	if results == nil {
		results = []LintResult{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}
