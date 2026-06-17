package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/davesade/kilolock/internal/plan"
)

type apiClient struct {
	baseURL  string
	username string
	password string
	bearer   string
}

func newAPIClientFromBackend(cwd string) (*apiClient, error) {
	bi, err := plan.DiscoverBackend(cwd)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(bi.Address)
	if err != nil {
		return nil, fmt.Errorf("parse backend address: %w", err)
	}
	base := (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
	c := &apiClient{
		baseURL:  strings.TrimRight(base, "/"),
		username: strings.TrimSpace(bi.Username),
		password: strings.TrimSpace(bi.Password),
		bearer:   strings.TrimSpace(os.Getenv("KL_TOKEN")),
	}
	if c.username == "" && c.password == "" && u.User != nil {
		c.username = u.User.Username()
		c.password, _ = u.User.Password()
	}
	// Terraform-style backend auth env vars override discovered backend auth.
	if v := strings.TrimSpace(os.Getenv("TF_HTTP_USERNAME")); v != "" {
		c.username = v
	} else if v := strings.TrimSpace(os.Getenv("TF_HTTP_USER")); v != "" {
		c.username = v
	}
	if v := strings.TrimSpace(os.Getenv("TF_HTTP_PASSWORD")); v != "" {
		c.password = v
	}

	// Explicit kl env auth overrides everything above.
	if v := strings.TrimSpace(os.Getenv("KL_USERNAME")); v != "" {
		c.username = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_PASSWORD")); v != "" {
		c.password = v
	}
	return c, nil
}

func newAPIClient() (*apiClient, error) {
	if base := strings.TrimSpace(os.Getenv("KL_API_URL")); base != "" {
		username := strings.TrimSpace(os.Getenv("KL_USERNAME"))
		if username == "" {
			username = strings.TrimSpace(os.Getenv("TF_HTTP_USERNAME"))
			if username == "" {
				username = strings.TrimSpace(os.Getenv("TF_HTTP_USER"))
			}
		}
		password := strings.TrimSpace(os.Getenv("KL_PASSWORD"))
		if password == "" {
			password = strings.TrimSpace(os.Getenv("TF_HTTP_PASSWORD"))
		}
		return &apiClient{
			baseURL:  strings.TrimRight(base, "/"),
			username: username,
			password: password,
			bearer:   strings.TrimSpace(os.Getenv("KL_TOKEN")),
		}, nil
	}
	return newAPIClientFromBackend(".")
}

func (c *apiClient) getJSON(ctx context.Context, path string, out any) error {
	return c.doJSON(ctx, http.MethodGet, path, "", nil, out)
}

func (c *apiClient) postJSON(ctx context.Context, path, stateName string, in, out any) error {
	return c.doJSON(ctx, http.MethodPost, path, stateName, in, out)
}

func (c *apiClient) doJSON(ctx context.Context, method, path, stateName string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(stateName) != "" {
		req.Header.Set("X-Kl-State-Name", strings.TrimSpace(stateName))
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	} else if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
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

func (c *apiClient) getBytes(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	} else if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: %s", req.Method, path, resp.Status)
	}
	return raw, nil
}
