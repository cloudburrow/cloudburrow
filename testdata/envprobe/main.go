// Command envprobe is a Cloud Run service that builds its Cloud Storage and
// Pub/Sub clients the way an application on Cloud Run does — with no
// options at all — and uses them (#576).
//
// It sets no endpoint, no credentials and no project of its own. Whatever
// it reaches is what the environment CloudBurrow injected into the revision
// points it at: STORAGE_EMULATOR_HOST, PUBSUB_EMULATOR_HOST and
// GOOGLE_CLOUD_PROJECT. Without them, the clients would look for
// googleapis.com and real credentials, and the request would fail.
//
// /adc (#681, adc.go) uses the rest of what is injected: credentials from
// the metadata server at GCE_METADATA_HOST, and KMS, Scheduler and Logging
// at their CLOUDBURROW_*_ENDPOINT.
//
// /bigquery (#874, bigquery.go) sends BigQuery requests BigQuery refuses to
// the injected CLOUDBURROW_BIGQUERY_ENDPOINT and reports each status.
// `envprobe bigquery <query>` runs the same probe once, with <query> as
// its URL query, prints what it reported and exits: a one-off pod, with no
// Cloud Run to route to it, runs it so (#902).
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "bigquery" {
		q, err := url.ParseQuery(os.Args[2])
		if err == nil {
			var out string
			if out, err = bigqueryProbe(context.Background(), q); err == nil {
				fmt.Printf("BIGQUERY PROBE: OK %s\n", out)
				return
			}
		}
		fmt.Printf("BIGQUERY PROBE: FAIL %v\n", err)
		os.Exit(1)
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		out, err := probe(r.Context(), r.URL.Query().Get("bucket"), r.URL.Query().Get("topic"))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "ENV PROBE: FAIL %v\n", err)
			return
		}
		fmt.Fprintf(w, "ENV PROBE: OK %s\n", out)
	})
	http.HandleFunc("/adc", func(w http.ResponseWriter, r *http.Request) {
		out, err := adcProbe(r.Context(), r.URL.Query())
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "ADC PROBE: FAIL %v\n", err)
			return
		}
		fmt.Fprintf(w, "ADC PROBE: OK %s\n", out)
	})
	http.HandleFunc("/bigquery", func(w http.ResponseWriter, r *http.Request) {
		out, err := bigqueryProbe(r.Context(), r.URL.Query())
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "BIGQUERY PROBE: FAIL %v\n", err)
			return
		}
		fmt.Fprintf(w, "BIGQUERY PROBE: OK %s\n", out)
	})
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// probe lists a bucket and publishes to a topic with clients built with no
// options, and reports what it used.
func probe(ctx context.Context, bucket, topic string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	sc, err := storage.NewClient(ctx)
	if err != nil {
		return "", fmt.Errorf("storage.NewClient: %w", err)
	}
	defer sc.Close()
	var objects []string
	it := sc.Bucket(bucket).Objects(ctx, nil)
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return "", fmt.Errorf("list bucket %s: %w", bucket, err)
		}
		objects = append(objects, attrs.Name)
	}

	project := os.Getenv("GOOGLE_CLOUD_PROJECT")
	pc, err := pubsub.NewClient(ctx, project)
	if err != nil {
		return "", fmt.Errorf("pubsub.NewClient: %w", err)
	}
	defer pc.Close()
	pub := pc.Publisher(topic)
	defer pub.Stop()
	id, err := pub.Publish(ctx, &pubsub.Message{Data: []byte("from-the-revision")}).Get(ctx)
	if err != nil {
		return "", fmt.Errorf("publish to %s: %w", topic, err)
	}
	return fmt.Sprintf("objects=%s message=%s project=%s storage=%s pubsub=%s",
		strings.Join(objects, ","), id, project, os.Getenv("STORAGE_EMULATOR_HOST"), os.Getenv("PUBSUB_EMULATOR_HOST")), nil
}
