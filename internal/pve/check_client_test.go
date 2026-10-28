package pve

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestNewCheckClientInsecureIsExplicitAndIsolated(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api2/json/version" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "PVEAPIToken=check@pve!test=synthetic-secret" {
			t.Error("missing synthetic token authorization")
		}
		io.WriteString(w, `{"data":{"version":"test"}}`)
	}))
	defer server.Close()
	cfg := Config{Endpoint: server.URL, TokenID: "check@pve!test", TokenSecret: "synthetic-secret"}
	for _, caFile := range []string{"", filepath.Join(t.TempDir(), "missing-ca.pem")} {
		cfg.CAFile = caFile
		client, err := NewCheckClient(cfg, true)
		if err != nil {
			t.Fatalf("explicit insecure check should ignore CA file: %v", err)
		}
		defer client.Close()
		var response map[string]string
		if err := client.getData(context.Background(), "/api2/json/version", &response); err != nil || response["version"] != "test" {
			t.Fatalf("explicit insecure request failed: %v", err)
		}
		if cfg.CAFile != caFile {
			t.Fatal("constructor changed the caller's configuration")
		}
	}
	// An insecure check must not affect later clients.
	cfg.CAFile = ""
	for _, factory := range []struct {
		name string
		new  func(Config) (*Client, error)
	}{
		{"secure check", func(cfg Config) (*Client, error) { return NewCheckClient(cfg, false) }},
		{"normal client", NewClient},
	} {
		t.Run(factory.name, func(t *testing.T) {
			client, err := factory.new(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var response map[string]string
			err = client.getData(context.Background(), "/api2/json/version", &response)
			var certificateErr *tls.CertificateVerificationError
			if !errors.As(err, &certificateErr) {
				t.Fatalf("secure client must reject the untrusted certificate, got %v", err)
			}
		})
	}
	cfg.CAFile = filepath.Join(t.TempDir(), "missing-ca.pem")
	if client, err := NewCheckClient(cfg, false); err == nil || client != nil {
		t.Fatal("secure check ignored a missing CA file")
	}
}

func TestNewCheckClientInsecureStillValidatesConfig(t *testing.T) {
	for _, cfg := range []Config{
		{Endpoint: "http://pve.example.test", TokenID: "check@pve!test", TokenSecret: "synthetic-secret"},
		{Endpoint: "https://pve.example.test", TokenID: "", TokenSecret: "synthetic-secret"},
		{Endpoint: "https://pve.example.test", TokenID: "check@pve!test", TokenSecret: "line\nbreak"},
	} {
		if client, err := NewCheckClient(cfg, true); err == nil || client != nil {
			t.Fatal("insecure override bypassed normal connection validation")
		}
	}
}
