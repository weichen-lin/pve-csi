package pve

import "net/http"

// NewCheckClient optionally skips certificate verification for explicitly
// requested diagnostics. Controller clients must continue to use NewClient.
func NewCheckClient(cfg Config, insecure bool) (*Client, error) {
	if !insecure {
		return NewClient(cfg)
	}
	// An explicit insecure check does not need a readable or valid CA file.
	// Config is a value copy, so the caller's normal configuration is unchanged.
	cfg.CAFile = ""
	client, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	// NewClient owns both this cloned transport and its TLS configuration. Set
	// the diagnostic override before any request, without changing global state.
	transport := client.httpClient.Transport.(*http.Transport)
	transport.TLSClientConfig.InsecureSkipVerify = true
	return client, nil
}
