// Command eventprobe is the Cloud Run fixture of the event trigger tests
// (#1414): it records every CloudEvent POSTed to it, its ce-* headers, its
// Authorization header and its data, and GET /events answers them as JSON.
// PROBE_STDOUT and PROBE_STDERR, when set, are written to standard output
// and standard error as it starts: the Kubernetes Engine logging test's
// lines (#2100).
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
)

type event struct {
	Headers map[string]string `json:"headers"`
	Data    string            `json:"data"`
	// Path is the request's path: an Eventarc trigger's destination can
	// name one (#1877).
	Path string `json:"path"`
	// DataBase64 is the data as sent, for a binary one: Firestore's
	// events are protobuf (#1877).
	DataBase64 string `json:"dataBase64"`
}

func main() {
	if v := os.Getenv("PROBE_STDOUT"); v != "" {
		fmt.Fprintln(os.Stdout, v)
	}
	if v := os.Getenv("PROBE_STDERR"); v != "" {
		fmt.Fprintln(os.Stderr, v)
	}
	var mu sync.Mutex
	var events []event
	http.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(events)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST a CloudEvent", http.StatusMethodNotAllowed)
			return
		}
		b, _ := io.ReadAll(r.Body)
		e := event{Headers: map[string]string{}, Data: string(b), Path: r.URL.Path, DataBase64: base64.StdEncoding.EncodeToString(b)}
		for k, v := range r.Header {
			if lk := strings.ToLower(k); strings.HasPrefix(lk, "ce-") || lk == "authorization" || lk == "content-type" {
				e.Headers[lk] = strings.Join(v, ",")
			}
		}
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/fail") {
			// A destination that fails every delivery, for the
			// dead-letter tests (#1997): recorded, then refused.
			http.Error(w, "this path fails every event", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
