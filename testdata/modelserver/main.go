// Command modelserver is a Vertex AI custom serving container fixture for
// the Model Registry and Endpoints (#1940).
//
// It does what Google's custom container requirements ask of a container
// whose model has an artifactUri: at startup it downloads model.json from
// the directory AIP_STORAGE_URI names, then serves AIP_HEALTH_ROUTE and
// AIP_PREDICT_ROUTE on AIP_HTTP_PORT. The model is a linear one,
// prediction = weight * instance + bias, so a prediction proves which
// artifacts were loaded. With parameters {"report": "env"} each
// prediction is instead the AIP_* variables the container was given, and
// with {"report": "model"} the loaded model's name. Cloud Storage is read through its JSON API at
// STORAGE_EMULATOR_HOST (CloudBurrow sets it on every workload), with the
// standard library only.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
)

type model struct {
	Weight float64 `json:"weight"`
	Bias   float64 `json:"bias"`
	Name   string  `json:"name"`
}

func main() {
	port := envOr("AIP_HTTP_PORT", "8080")
	health := os.Getenv("AIP_HEALTH_ROUTE")
	predict := os.Getenv("AIP_PREDICT_ROUTE")
	if health == "" || predict == "" {
		log.Fatal("AIP_HEALTH_ROUTE and AIP_PREDICT_ROUTE must be set")
	}
	m := model{Weight: 1, Name: "no-artifacts"}
	if uri := os.Getenv("AIP_STORAGE_URI"); uri != "" {
		var err error
		if m, err = load(uri); err != nil {
			log.Fatalf("loading the model from %s: %v", uri, err)
		}
	}
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "AIP_") {
			env[k] = v
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc(health, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc(predict, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			// Each instance is a number, or an object {"x": number}
			// (Model Monitoring's instances name their features, #2214).
			Instances  []xInstance `json:"instances"`
			Parameters struct {
				Report string `json:"report"`
			} `json:"parameters"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Instances) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"error":"instances must be a non-empty list of numbers"}`)
			return
		}
		var out struct {
			Predictions []any `json:"predictions"`
		}
		for _, in := range req.Instances {
			x := float64(in)
			switch req.Parameters.Report {
			case "env":
				out.Predictions = append(out.Predictions, env)
			case "model":
				out.Predictions = append(out.Predictions, m.Name)
			default:
				out.Predictions = append(out.Predictions, m.Weight*x+m.Bias)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	log.Fatal(http.ListenAndServe("0.0.0.0:"+port, mux))
}

// xInstance is an instance: a number, or {"x": number}.
type xInstance float64

func (x *xInstance) UnmarshalJSON(b []byte) error {
	var n float64
	if err := json.Unmarshal(b, &n); err == nil {
		*x = xInstance(n)
		return nil
	}
	var o struct {
		X *float64 `json:"x"`
	}
	if err := json.Unmarshal(b, &o); err != nil || o.X == nil {
		return fmt.Errorf("instance %s is neither a number nor {\"x\": number}", b)
	}
	*x = xInstance(*o.X)
	return nil
}

func load(uri string) (model, error) {
	rest, ok := strings.CutPrefix(uri, "gs://")
	bucket, dir, _ := strings.Cut(rest, "/")
	if !ok || bucket == "" {
		return model{}, fmt.Errorf("%q is not a gs:// URI", uri)
	}
	host := os.Getenv("STORAGE_EMULATOR_HOST")
	if host == "" {
		return model{}, fmt.Errorf("STORAGE_EMULATOR_HOST is not set")
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	obj := strings.TrimSuffix(dir, "/") + "/model.json"
	resp, err := http.Get(host + "/storage/v1/b/" + url.PathEscape(bucket) + "/o/" + url.PathEscape(obj) + "?alt=media")
	if err != nil {
		return model{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return model{}, fmt.Errorf("GET gs://%s/%s: %d %s", bucket, obj, resp.StatusCode, b)
	}
	var m model
	return m, json.Unmarshal(b, &m)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
