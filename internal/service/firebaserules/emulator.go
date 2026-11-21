package firebaserules

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Emulator is the Firestore emulator, through the instance's Firestore
// front: one HTTP call.
type Emulator interface {
	Do(ctx context.Context, method, path string, header http.Header, body []byte) (int, []byte, error)
}

// HTTPEmulator is the Firestore emulator at Base (http://host:port), read
// at each call, since the Firestore tunnel binds after this API starts.
type HTTPEmulator struct {
	Base   func() string
	Client *http.Client
}

// Do makes one call.
func (e HTTPEmulator) Do(ctx context.Context, method, path string, header http.Header, body []byte) (int, []byte, error) {
	base := ""
	if e.Base != nil {
		base = e.Base()
	}
	if base == "" {
		return 0, nil, fmt.Errorf("the instance's Firestore has not started")
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return 0, nil, fmt.Errorf("the instance's Firestore address %q is not a URL", base)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c := e.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, b, err
}

// checkProject is the scratch project rulesets are compiled on.
const checkProject = "cloudburrow-rules-check"

// OpenRules are the rules of a project with no cloud.firestore release:
// everything allowed, as CloudBurrow's Firestore always served.
const OpenRules = `rules_version = '2';
service cloud.firestore {
  match /databases/{database}/documents {
    match /{document=**} {
      allow read, write: if true;
    }
  }
}
`

// CompileError is the emulator's refusal of a ruleset's source.
type CompileError struct{ Message string }

func (e *CompileError) Error() string { return e.Message }

// googleError is a Google JSON error's message.
func googleError(b []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return string(b)
}

// ownerHeader is the emulator's owner credential, which bypasses rules.
func ownerHeader() http.Header {
	return http.Header{"Authorization": {"Bearer owner"}}
}

// setRules sets project's rules in the emulator
// (PUT /emulator/v1/projects/P:securityRules).
func setRules(ctx context.Context, emu Emulator, project, source string) error {
	body, _ := json.Marshal(map[string]any{"rules": map[string]any{
		"files": []map[string]string{{"name": "firestore.rules", "content": source}}}})
	code, b, err := emu.Do(ctx, http.MethodPut, "/emulator/v1/projects/"+url.PathEscape(project)+":securityRules", ownerHeader(), body)
	if err != nil {
		return err
	}
	switch {
	case code == http.StatusBadRequest:
		return &CompileError{Message: googleError(b)}
	case code/100 != 2:
		return fmt.Errorf("the Firestore emulator answered %d: %s", code, googleError(b))
	}
	return nil
}

// clearProject deletes a scratch project's documents.
func clearProject(ctx context.Context, emu Emulator, project string) {
	_, _, _ = emu.Do(ctx, http.MethodDelete, "/emulator/v1/projects/"+url.PathEscape(project)+"/databases/(default)/documents", ownerHeader(), nil)
}
