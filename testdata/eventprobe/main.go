// Command eventprobe is the Cloud Run fixture of the event trigger tests
// (#1414): it records every CloudEvent POSTed to it, its ce-* headers, its
// Authorization header and its data, and GET /events answers them as JSON.
package main

import (
	"encoding/json"
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
}

func main() {
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
		e := event{Headers: map[string]string{}, Data: string(b)}
		for k, v := range r.Header {
			if lk := strings.ToLower(k); strings.HasPrefix(lk, "ce-") || lk == "authorization" || lk == "content-type" {
				e.Headers[lk] = strings.Join(v, ",")
			}
		}
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
