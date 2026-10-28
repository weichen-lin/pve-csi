package pve

import "net/http"

// NewCheckClient permits diagnostic TLS bypass; controllers must use NewClient.
func NewCheckClient(cfg Config, insecure bool) (*Client, error) {
	if !insecure {
		return NewClient(cfg)
	}
	// Ignore CA-file configuration only for this diagnostic client.
	cfg.CAFile = ""
	client, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	// The transport and TLS config belong to this client.
	transport := client.httpClient.Transport.(*http.Transport)
	transport.TLSClientConfig.InsecureSkipVerify = true
	return client, nil
}
