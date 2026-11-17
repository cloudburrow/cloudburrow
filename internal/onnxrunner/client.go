package onnxrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/onnxmodel"
)

// Client calls a runner's Server at a base URL (http://host:port).
type Client struct {
	base string
	hc   *http.Client
}

// NewClient calls the runner at base. A nil hc is a client with a timeout
// long enough to send a 450 MB model and run it.
func NewClient(base string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	return &Client{base: strings.TrimRight(base, "/"), hc: hc}
}

// Base is the runner's URL.
func (c *Client) Base() string { return c.base }

// Load sends the model, whose hex SHA-256 is sum, and returns its inputs
// and outputs as the runner's ONNX Runtime reads them.
func (c *Client) Load(ctx context.Context, sum string, model []byte) (inputs, outputs []onnxmodel.Port, err error) {
	var p ports
	if err := c.do(ctx, http.MethodPut, "/v1/models/"+sum, "application/octet-stream", model, &p); err != nil {
		return nil, nil, err
	}
	return p.Inputs, p.Outputs, nil
}

// Run runs the model whose hex SHA-256 is sum on in, one Tensor for each
// of inputs, and returns one Tensor for each of outputs. When the runner
// does not hold the model (a restart, or its cache evicted it), model is
// asked for the bytes, which are sent before the run is tried again.
func (c *Client) Run(ctx context.Context, sum string, model func() ([]byte, error), inputs []onnxmodel.Port,
	in []onnxmodel.Tensor, outputs []onnxmodel.Port) ([]onnxmodel.Tensor, error) {
	body, err := json.Marshal(runRequest{Inputs: inputs, Outputs: outputs, Tensors: toWire(in)})
	if err != nil {
		return nil, err
	}
	var res runResponse
	err = c.do(ctx, http.MethodPost, "/v1/models/"+sum+":run", "application/json", body, &res)
	if e, ok := err.(*Error); ok && e.Status == http.StatusNotFound {
		data, merr := model()
		if merr != nil {
			return nil, merr
		}
		if _, _, err := c.Load(ctx, sum, data); err != nil {
			return nil, err
		}
		err = c.do(ctx, http.MethodPost, "/v1/models/"+sum+":run", "application/json", body, &res)
	}
	if err != nil {
		return nil, err
	}
	if len(res.Tensors) != len(outputs) {
		return nil, fmt.Errorf("onnxrunner: %d tensors for %d outputs", len(res.Tensors), len(outputs))
	}
	return fromWire(res.Tensors), nil
}

// do sends one request. A refusal is an *Error; anything else (the runner
// could not be reached, or answered what is not its API) a plain error.
func (c *Client) do(ctx context.Context, method, path, contentType string, body []byte, out any) error {
	if c.base == "" {
		return fmt.Errorf("onnxrunner: no runner address")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("the ONNX runner at %s could not be reached: %w", c.base, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("the ONNX runner at %s: %w", c.base, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e errorBody
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			return fmt.Errorf("the ONNX runner at %s answered HTTP %d: %.200s", c.base, resp.StatusCode, b)
		}
		return &Error{Status: resp.StatusCode, Message: e.Error}
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("the ONNX runner at %s answered what is not its API: %w", c.base, err)
	}
	return nil
}
