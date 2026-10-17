// Package function is the fixture used to verify Google Functions Framework
// and Buildpacks support.
//
// It is an ordinary Functions Framework function: no CloudBurrow-specific
// code, because the point is that a normal function runs unchanged.
package function

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/GoogleCloudPlatform/functions-framework-go/functions"
	"github.com/cloudevents/sdk-go/v2/event"
)

func init() {
	functions.HTTP("Hello", hello)
	functions.CloudEvent("Event", handleEvent)
	functions.HTTP("Secrets", secrets)
}

// secrets answers what the platform mounted: the environment variable
// SECRET_ENV names, and the file SECRET_FILE names, as a function reads a
// secret its deployment exposes.
func secrets(w http.ResponseWriter, _ *http.Request) {
	file, err := os.ReadFile(os.Getenv("SECRET_FILE"))
	out := map[string]string{"env": os.Getenv(os.Getenv("SECRET_ENV")), "file": string(file)}
	if err != nil {
		out["fileError"] = err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// hello answers an HTTP request, echoing a name when given one.
func hello(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	// A malformed body is not fatal: the function still answers, which is what
	// lets the test distinguish "no name given" from "handler broken".
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Name == "" {
		body.Name = "world"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"greeting": "hello " + body.Name})
}

// handleEvent receives a CloudEvent in the Google schema.
func handleEvent(ctx context.Context, e event.Event) error {
	var data map[string]any
	if err := e.DataAs(&data); err != nil {
		return fmt.Errorf("decode CloudEvent data: %w", err)
	}
	fmt.Printf("received CloudEvent type=%s subject=%s data=%v\n", e.Type(), e.Subject(), data)
	return nil
}
