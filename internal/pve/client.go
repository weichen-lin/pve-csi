package pve

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config contains connection settings only, never a queried VM or allocation owner.
type Config struct {
	Endpoint    string
	TokenID     string
	TokenSecret string
	CAFile      string
}

func (cfg Config) Normalize() (Config, error) {
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	cfg.TokenID = strings.TrimSpace(cfg.TokenID)
	cfg.CAFile = strings.TrimSpace(cfg.CAFile)
	for _, required := range []struct{ name, value string }{
		{"PVE_ENDPOINT", cfg.Endpoint}, {"PVE_TOKEN_ID", cfg.TokenID}, {"PVE_TOKEN_SECRET", cfg.TokenSecret},
	} {
		if required.value == "" {
			return Config{}, fmt.Errorf("%s is required", required.name)
		}
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return Config{}, errors.New("PVE_ENDPOINT must be an HTTPS URL with a host")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return Config{}, errors.New("PVE_ENDPOINT must contain only the HTTPS host and optional port")
	}
	if strings.ContainsAny(cfg.TokenID, "\r\n") || strings.ContainsAny(cfg.TokenSecret, "\r\n") {
		return Config{}, errors.New("PVE_TOKEN_ID and PVE_TOKEN_SECRET must not contain line breaks")
	}
	cfg.Endpoint = u.Scheme + "://" + u.Host
	return cfg, nil
}

type Client struct {
	httpClient *http.Client
	config     Config
}

func NewClient(cfg Config) (*Client, error) {
	normalized, err := cfg.Normalize()
	if err != nil {
		return nil, err
	}
	client, err := newHTTPClient(normalized.CAFile)
	if err != nil {
		return nil, err
	}
	return &Client{httpClient: client, config: normalized}, nil
}

func (c *Client) Close() { c.httpClient.CloseIdleConnections() }

func newHTTPClient(caFile string) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read PVE_CA_FILE: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("PVE_CA_FILE contains no usable PEM certificate")
		}
		tlsConfig.RootCAs = roots
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

// PVE wraps each successful API result in a JSON object with a data field.

func (c *Client) getData(ctx context.Context, apiPath string, target any) error {
	return c.requestData(ctx, http.MethodGet, apiPath, nil, target)
}

func (c *Client) postData(ctx context.Context, apiPath string, form url.Values, target any) error {
	return c.requestData(ctx, http.MethodPost, apiPath, form, target)
}

func (c *Client) requestData(ctx context.Context, method, apiPath string, form url.Values, target any) error {
	var requestBody io.Reader
	if form != nil {
		requestBody = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.config.Endpoint+apiPath, requestBody)
	if err != nil {
		return fmt.Errorf("create PVE request: %w", err)
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.config.TokenID+"="+c.config.TokenSecret)
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("PVE %s %s: %w", method, apiPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Do not print a response body: a server could echo request credentials.
		return &HTTPError{Method: method, Path: apiPath, StatusCode: resp.StatusCode}
	}

	const maxResponseBytes = 8 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read PVE %s %s response: %w", method, apiPath, err)
	}
	if len(body) > maxResponseBytes {
		return fmt.Errorf("PVE %s %s response exceeds 8 MiB", method, apiPath)
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode PVE %s %s response: %w", method, apiPath, err)
	}
	data := strings.TrimSpace(string(envelope.Data))
	if data == "" || data == "null" {
		return fmt.Errorf("PVE %s %s response is missing data", method, apiPath)
	}
	if err := json.Unmarshal(envelope.Data, target); err != nil {
		return fmt.Errorf("decode PVE %s %s data: %w", method, apiPath, err)
	}
	return nil
}

// HTTPError preserves status classification without retaining a server body
// or credentials.
type HTTPError struct {
	Method     string
	Path       string
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("PVE %s %s: HTTP %d (%s)", e.Method, e.Path, e.StatusCode, http.StatusText(e.StatusCode))
}
