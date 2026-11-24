package logagent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/encoding/protojson"
)

// The service account files a pod is given.
const (
	saToken = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCA    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// InCluster reads the configuration and the node's pods from the API
// server, as the pod's service account.
type InCluster struct {
	Server string // https://host:port
	Node   string
	Client *http.Client
	// Token reads the service account's token, which the kubelet rotates.
	Token func() (string, error)
}

// NewInCluster is the client of the API server a pod is given in
// KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT.
func NewInCluster(node string) (*InCluster, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set: not in a pod")
	}
	ca, err := os.ReadFile(saCA)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("%s holds no certificate", saCA)
	}
	return &InCluster{
		Server: "https://" + net.JoinHostPort(host, port), Node: node,
		Client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}},
		Token: func() (string, error) {
			b, err := os.ReadFile(saToken)
			return strings.TrimSpace(string(b)), err
		},
	}, nil
}

func (k *InCluster) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.Server+path, nil)
	if err != nil {
		return err
	}
	tok, err := k.Token()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := k.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s: %s", path, resp.Status, bytes.TrimSpace(b))
	}
	return json.Unmarshal(b, out)
}

// Config reads the ConfigMap.
func (k *InCluster) Config(ctx context.Context) (Config, error) {
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := k.get(ctx, "/api/v1/namespaces/"+Namespace+"/configmaps/"+ConfigMap, &cm); err != nil {
		return Config{}, err
	}
	return ParseConfig(cm.Data[ConfigKey])
}

// Pods lists the node's pods.
func (k *InCluster) Pods(ctx context.Context) ([]Pod, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name      string            `json:"name"`
				Namespace string            `json:"namespace"`
				UID       string            `json:"uid"`
				Labels    map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := k.get(ctx, "/api/v1/pods?fieldSelector="+url.QueryEscape("spec.nodeName="+k.Node), &list); err != nil {
		return nil, err
	}
	out := make([]Pod, 0, len(list.Items))
	for _, p := range list.Items {
		out = append(out, Pod{Namespace: p.Metadata.Namespace, Name: p.Metadata.Name, UID: p.Metadata.UID, Labels: p.Metadata.Labels})
	}
	return out, nil
}

// Writer is the Cloud Logging API's entries.write over REST
// (https://docs.cloud.google.com/logging/docs/reference/v2/rest/v2/entries/write).
type Writer struct {
	// Endpoint is the instance's Cloud Logging, http://host:port. It is
	// never Google's: NewWriter refuses an empty one and any Google host.
	Endpoint string
	Client   *http.Client
}

// NewWriter is a writer to endpoint, which must be the instance's.
func NewWriter(endpoint string) (*Writer, error) {
	u, err := url.Parse(endpoint)
	if endpoint == "" || err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("the Cloud Logging endpoint %q is not an http(s) address: the agent writes only to the instance's", endpoint)
	}
	h := strings.ToLower(u.Hostname())
	if strings.HasSuffix(h, "googleapis.com") || strings.HasSuffix(h, "google.com") {
		return nil, fmt.Errorf("the Cloud Logging endpoint %q is Google's: the agent writes only to the instance's", endpoint)
	}
	return &Writer{Endpoint: strings.TrimSuffix(endpoint, "/"), Client: &http.Client{Timeout: 30 * time.Second}}, nil
}

// Write sends one request. A refusal that a retry cannot change (a 4xx
// other than 429, 401 and 403, which an IAM change can lift) is a
// PermanentError.
func (w *Writer) Write(ctx context.Context, req *loggingpb.WriteLogEntriesRequest) error {
	body, err := protojson.Marshal(req)
	if err != nil {
		return &PermanentError{Err: err}
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, w.Endpoint+"/v2/entries:write", bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := w.Client.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode/100 == 2:
		return nil
	case resp.StatusCode/100 == 4 && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden:
		return &PermanentError{Err: fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(b))}
	default:
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(b))
	}
}
