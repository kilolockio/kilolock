package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

type controlAPIClient struct {
	baseURL string
	bearer  string
}

func newControlAPIClient() *controlAPIClient {
	base := strings.TrimSpace(os.Getenv("KL_CONTROL_API_URL"))
	if base == "" {
		base = "http://127.0.0.1:8090"
	}
	return &controlAPIClient{
		baseURL: strings.TrimRight(base, "/"),
		bearer:  strings.TrimSpace(os.Getenv("KL_CONTROL_TOKEN")),
	}
}

func (c *controlAPIClient) getJSON(ctx context.Context, path string, out any) error {
	return c.doJSON(ctx, http.MethodGet, path, nil, out)
}

func (c *controlAPIClient) postJSON(ctx context.Context, path string, in, out any) error {
	return c.doJSON(ctx, http.MethodPost, path, in, out)
}

func (c *controlAPIClient) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if out != nil && len(body) > 0 {
			_ = json.Unmarshal(body, out)
		}
		return fmt.Errorf("%s %s: %s (%s)", req.Method, path, resp.Status, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
