// Command worker is the acceptance-workflow worker: it receives a Pub/Sub push,
// reads the named object from Cloud Storage, and writes a result object back.
//
// It uses the official Cloud Storage SDK rather than raw HTTP, because the
// point of the acceptance workflow is that ordinary Google client code runs
// unchanged against CloudBurrow.
package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

type pushEnvelope struct {
	Message struct {
		Data       string            `json:"data"`
		Attributes map[string]string `json:"attributes"`
	} `json:"message"`
}

func main() {
	// Deliberately NOT named STORAGE_EMULATOR_HOST.
	//
	// Setting that variable puts the official client into emulator mode, where
	// it reads objects via the virtual-host path /{bucket}/{object}. The
	// backend matches that path against its single -public-host value, which is
	// necessarily the *host* address, so an in-cluster read returns 404.
	//
	// With only an explicit endpoint, the same official client uses the JSON
	// API paths (/storage/v1/... and /download/storage/v1/...), which work
	// identically from the host and from inside the cluster.
	endpoint := os.Getenv("STORAGE_ENDPOINT")
	bucket := os.Getenv("BUCKET")
	if endpoint == "" || bucket == "" {
		log.Fatal("STORAGE_ENDPOINT and BUCKET are required")
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var env pushEnvelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		raw, err := base64.StdEncoding.DecodeString(env.Message.Data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		object := strings.TrimSpace(string(raw))
		log.Printf("received push for object %q", object)

		ctx := r.Context()
		client, err := storage.NewClient(ctx,
			option.WithoutAuthentication(),
			option.WithEndpoint(endpoint+"/storage/v1/"))
		if err != nil {
			log.Printf("storage client: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer client.Close()

		bh := client.Bucket(bucket)
		rd, err := bh.Object(object).NewReader(ctx)
		if err != nil {
			log.Printf("read %s: %v", object, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		body, err := io.ReadAll(rd)
		rd.Close()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		result := strings.ToUpper(string(body))
		wr := bh.Object("result.txt").NewWriter(ctx)
		if _, err := wr.Write([]byte(result)); err != nil {
			log.Printf("write result: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := wr.Close(); err != nil {
			log.Printf("close result: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("wrote result.txt (%d bytes)", len(result))
		w.WriteHeader(http.StatusNoContent)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("worker listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
