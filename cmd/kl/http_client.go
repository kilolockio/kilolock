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
)

type apiClient struct {
	baseURL          string
	username         string
	password         string
	bearer           string
	defaultStateName string
}

func newAPIClientFromBackend(cwd string) (*apiClient, error) {
	bi, err := discoverLiveBackend(cwd)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(bi.Address)
	if err != nil {
		return nil, fmt.Errorf("parse backend address: %w", err)
	}
	base := (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
	c := &apiClient{
		baseURL:          strings.TrimRight(base, "/"),
		username:         strings.TrimSpace(bi.Username),
		password:         strings.TrimSpace(bi.Password),
		defaultStateName: strings.TrimSpace(bi.StateName),
	}
	if c.username == "" && c.password == "" && u.User != nil {
		c.username = u.User.Username()
		c.password, _ = u.User.Password()
	}
	applyAuthEnvOverrides(c, "")
	return c, nil
}

func newAPIClient() (*apiClient, error) {
	return newAPIClientWithToken(".", "")
}

func newAPIClientWithToken(cwd, explicitToken string) (*apiClient, error) {
	if stateURL := strings.TrimSpace(os.Getenv("KL_STATE_URL")); stateURL != "" {
		target, _, err := stateTargetFromAddress(stateURL)
		if err != nil {
			return nil, fmt.Errorf("parse KL_STATE_URL: %w", err)
		}
		return newAPIClientForTarget(cwd, target, explicitToken)
	}
	if base := strings.TrimSpace(os.Getenv("KL_API_URL")); base != "" {
		c := &apiClient{baseURL: strings.TrimRight(base, "/")}
		applyAuthEnvOverrides(c, explicitToken)
		return c, nil
	}
	c, err := newAPIClientFromBackend(cwd)
	if err != nil {
		return nil, err
	}
	if token := strings.TrimSpace(explicitToken); token != "" {
		c.bearer = token
	}
	return c, nil
}

func newAPIClientForTarget(cwd string, target stateTarget, explicitToken string) (*apiClient, error) {
	if strings.TrimSpace(target.BaseURL) != "" {
		c := &apiClient{
			baseURL:          strings.TrimRight(target.BaseURL, "/"),
			username:         strings.TrimSpace(target.Username),
			password:         strings.TrimSpace(target.Password),
			defaultStateName: strings.TrimSpace(target.StateName),
		}
		applyAuthEnvOverrides(c, explicitToken)
		return c, nil
	}
	if base := strings.TrimSpace(os.Getenv("KL_API_URL")); base != "" {
		c := &apiClient{baseURL: strings.TrimRight(base, "/")}
		applyAuthEnvOverrides(c, explicitToken)
		return c, nil
	}
	c, err := newAPIClientFromBackend(cwd)
	if err != nil {
		return nil, err
	}
	if token := strings.TrimSpace(explicitToken); token != "" {
		c.bearer = token
	}
	return c, nil
}

func applyAuthEnvOverrides(c *apiClient, explicitToken string) {
	c.bearer = strings.TrimSpace(explicitToken)
	if c.bearer == "" {
		c.bearer = strings.TrimSpace(os.Getenv("KL_TOKEN"))
	}
	if v := strings.TrimSpace(os.Getenv("TF_HTTP_USERNAME")); v != "" {
		c.username = v
	} else if v := strings.TrimSpace(os.Getenv("TF_HTTP_USER")); v != "" {
		c.username = v
	}
	if v := strings.TrimSpace(os.Getenv("TF_HTTP_PASSWORD")); v != "" {
		c.password = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_USERNAME")); v != "" {
		c.username = v
	}
	if v := strings.TrimSpace(os.Getenv("KL_PASSWORD")); v != "" {
		c.password = v
	}
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
	if scoped := firstStateName(strings.TrimSpace(stateName), c.defaultStateName); scoped != "" {
		req.Header.Set("X-Kilolock-State-Name", scoped)
	}
	if scoped := firstStateName("", c.defaultStateName); scoped != "" {
		req.Header.Set("X-Kilolock-State-Name", scoped)
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

func firstStateName(parts ...string) string {
	for _, part := range parts {
		if v := strings.TrimSpace(part); v != "" {
			return v
		}
	}
	return ""
}
