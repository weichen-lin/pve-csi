package pveapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fakeTokenID = "test-user@pve!test-token"
const fakeSecret = "test-secret-do-not-print"

func testCAFile(t *testing.T, cert []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testConfig(endpoint string) Config {
	return Config{Endpoint: endpoint, TokenID: fakeTokenID, TokenSecret: fakeSecret}
}

func getData(ctx context.Context, transport *http.Client, cfg Config, path string, target any) error {
	client := &Client{httpClient: transport, config: cfg}
	return client.GetData(ctx, path, target)
}

func TestGetDataRejectsInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", 401, fakeSecret}, {"forbidden", 403, fakeSecret}, {"not found", 404, fakeSecret},
		{"invalid JSON", 200, fakeSecret}, {"missing data", 200, `{}`}, {"null data", 200, `{"data":null}`},
		{"trailing JSON", 200, `{"data":{}} {}`}, {"wrong data shape", 200, `{"data":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			client, err := newHTTPClient(testCAFile(t, server.Certificate().Raw))
			if err != nil {
				t.Fatal(err)
			}
			var target struct{ Version string }
			err = getData(context.Background(), client, testConfig(server.URL), "/api2/json/version", &target)
			if err == nil {
				t.Fatal("invalid response accepted")
			}
			if strings.Contains(err.Error(), fakeSecret) {
				t.Error("error contains response body secret")
			}
		})
	}
}

func TestClientTLSCancellationAndRedirect(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":{}}`) }))
	defer server.Close()
	client, err := newHTTPClient(testCAFile(t, server.Certificate().Raw))
	if err != nil {
		t.Fatal(err)
	}
	if client.Timeout != 10*time.Second {
		t.Errorf("timeout = %s, want 10s", client.Timeout)
	}
	t.Run("request timeout", func(t *testing.T) {
		slow := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer slow.Close()
		shortClient := *client
		shortClient.Timeout = 50 * time.Millisecond
		var target map[string]any
		if err := getData(context.Background(), &shortClient, testConfig(slow.URL), "/api2/json/version", &target); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected request deadline, got %v", err)
		}
	})
	t.Run("canceled request", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var target map[string]any
		if err := getData(ctx, client, testConfig(server.URL), "/api2/json/version", &target); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context cancellation, got %v", err)
		}
	})
	t.Run("hostname verification", func(t *testing.T) {
		transport := client.Transport.(*http.Transport).Clone()
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}
		badHostClient := *client
		badHostClient.Transport = transport
		var target map[string]any
		var hostnameErr x509.HostnameError
		if err := getData(context.Background(), &badHostClient, testConfig("https://wrong-host.invalid"), "/api2/json/version", &target); !errors.As(err, &hostnameErr) {
			t.Fatalf("expected hostname validation failure, got %v", err)
		}
	})
	t.Run("untrusted CA", func(t *testing.T) {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "unrelated-test-ca"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
		der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, priv)
		if err != nil {
			t.Fatal(err)
		}
		wrongClient, err := newHTTPClient(testCAFile(t, der))
		if err != nil {
			t.Fatal(err)
		}
		var target map[string]any
		var authorityErr x509.UnknownAuthorityError
		if err := getData(context.Background(), wrongClient, testConfig(server.URL), "/api2/json/version", &target); !errors.As(err, &authorityErr) {
			t.Fatalf("expected CA trust failure, got %v", err)
		}
	})
	t.Run("redirect not followed", func(t *testing.T) {
		var destinationCalls atomic.Int32
		destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls.Add(1); fmt.Fprint(w, `{"data":{}}`) }))
		defer destination.Close()
		redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
		defer redirect.Close()
		var target map[string]any
		if err := getData(context.Background(), client, testConfig(redirect.URL), "/api2/json/version", &target); err == nil {
			t.Error("redirect accepted as successful API response")
		}
		if destinationCalls.Load() != 0 {
			t.Fatal("redirect was followed, potentially forwarding token")
		}
	})
}

func TestClientRejectsMissingOrInvalidCA(t *testing.T) {
	for _, path := range []string{filepath.Join(t.TempDir(), "missing.pem"), testCAFile(t, []byte("invalid-certificate")), testCAFile(t, nil)} {
		if _, err := newHTTPClient(path); err == nil {
			t.Fatal("missing or invalid CA accepted")
		}
	}
}
