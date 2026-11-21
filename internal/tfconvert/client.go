package tfconvert

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls a converter's Server at a base URL (http://host:port).
type Client struct {
	base string
	hc   *http.Client
}

// NewClient calls the converter at base; a nil hc has a timeout long
// enough for TensorFlow to start and convert a 450 MB model.
func NewClient(base string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Minute}
	}
	return &Client{base: strings.TrimRight(base, "/"), hc: hc}
}

// Convert sends the SavedModel's files and returns its bundle
// (bundle.go), or a *Refusal.
func (c *Client) Convert(ctx context.Context, files []File) ([]byte, error) {
	if c.base == "" {
		return nil, &Refusal{Kind: "unavailable", Message: "no TensorFlow converter address"}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.Name, Mode: 0o644, Size: int64(len(f.Data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/convert", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, &Refusal{Kind: "unavailable", Message: "the TensorFlow converter could not be reached: " + err.Error()}
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, MaxModelBytes*2))
	if err != nil {
		return nil, &Refusal{Kind: "unavailable", Message: "the TensorFlow converter's answer was cut short: " + err.Error()}
	}
	switch res.StatusCode {
	case http.StatusOK:
		return body, nil
	case http.StatusUnprocessableEntity, http.StatusServiceUnavailable:
		return nil, decodeRefusal(body)
	}
	return nil, &Refusal{Kind: "unavailable", Message: fmt.Sprintf("the TensorFlow converter answered HTTP %d", res.StatusCode)}
}

// Run sends one request line to the TensorFlow runtime of the converted
// model whose bundle's SHA-256 is sum (predict.go), sending the bundle
// first when the converter does not hold it, and answers the answer line,
// or a *Refusal: of kind "run" for TensorFlow's own error.
func (c *Client) Run(ctx context.Context, sum string, bundle func() ([]byte, error), req []byte) ([]byte, error) {
	if c.base == "" {
		return nil, &Refusal{Kind: "unavailable", Message: "no TensorFlow converter address"}
	}
	for attempt := 0; ; attempt++ {
		code, body, err := c.post(ctx, "/v1/run?model="+sum, "application/json", req)
		if err != nil {
			return nil, err
		}
		switch {
		case code == http.StatusOK:
			return body, nil
		case code == http.StatusNotFound && attempt == 0:
			b, err := bundle()
			if err != nil {
				return nil, err
			}
			code, body, err := c.post(ctx, "/v1/models?model="+sum, "application/x-tar", b)
			if err != nil {
				return nil, err
			}
			if code != http.StatusNoContent {
				return nil, decodeRefusal(body)
			}
			continue
		case code == http.StatusUnprocessableEntity, code == http.StatusServiceUnavailable, code == http.StatusNotFound:
			return nil, decodeRefusal(body)
		}
		return nil, &Refusal{Kind: "unavailable", Message: fmt.Sprintf("the TensorFlow converter answered HTTP %d", code)}
	}
}

func (c *Client) post(ctx context.Context, path, typ string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", typ)
	res, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, &Refusal{Kind: "unavailable", Message: "the TensorFlow converter could not be reached: " + err.Error()}
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<30))
	if err != nil {
		return 0, nil, &Refusal{Kind: "unavailable", Message: "the TensorFlow converter's answer was cut short: " + err.Error()}
	}
	return res.StatusCode, b, nil
}
