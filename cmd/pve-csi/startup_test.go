package main

import (
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
)

func TestConfiguredOwnerVMID(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{"", false}, {"99", false}, {"100", true}, {"999999999", true}, {"1000000000", false},
		{"0100", false}, {"+100", false}, {"100.0", false}, {"99999999999999999999", false}, {" 7777 ", true},
	} {
		values := map[string]string{
			"PVE_ENDPOINT": "https://pve.example:8006", "PVE_TOKEN_ID": "test@pve!test", "PVE_TOKEN_SECRET": "test-secret",
			"PVE_NODE": "pve", "PVE_OWNER_VMID": tc.raw, "CSI_DRIVER_NAME": "example.csi",
		}
		_, err := LoadConfig(func(key string) string { return values[key] })
		if (err == nil) != tc.valid || (!tc.valid && !strings.Contains(err.Error(), "PVE_OWNER_VMID")) {
			t.Errorf("owner %q: %v; want valid=%t", tc.raw, err, tc.valid)
		}
	}
}

type startupOutput struct {
	strings.Builder
	cancel context.CancelFunc
}

func (w *startupOutput) Write(p []byte) (int, error) {
	n, err := w.Builder.Write(p)
	if strings.Contains(string(p), "CSI listening") {
		w.cancel()
	}
	return n, err
}

func TestControllerStartupChecksBeforeServing(t *testing.T) {
	const secret = "do-not-log-backend-secret"
	for _, tc := range []struct {
		name, hint string
		calls      int
	}{
		{"unauthenticated", "authentication failed", 1},
		{"forbidden", "denied the operation", 1},
		{"missing task access", "Sys.Audit on /nodes/pve", 1},
		{"missing inventory access", "VM.Audit with propagation", 2},
		{"inventory does not propagate", "VM.Audit with propagation", 2},
		{"node missing", "check_pve_node", 3},
		{"node malformed", "check_pve_node", 3},
		{"owner occupied", "reserved disk owner VMID", 4},
		{"request times out", "timed out", 1},
		{"ready", "", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "startup-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			socket := filepath.Join(dir, "csi.sock")
			var mu sync.Mutex
			var requests []string
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := os.Lstat(socket); !os.IsNotExist(err) {
					t.Error("CSI socket opened before startup checks completed")
				}
				if r.Method != "GET" {
					t.Error("startup attempted mutation")
				}
				mu.Lock()
				requests = append(requests, r.URL.RequestURI())
				mu.Unlock()
				if tc.name == "request times out" {
					<-r.Context().Done()
					return
				}
				if tc.name == "unauthenticated" || tc.name == "forbidden" {
					code := 401
					if tc.name == "forbidden" {
						code = 403
					}
					w.WriteHeader(code)
					fmt.Fprint(w, secret)
					return
				}
				switch r.URL.Path {
				case "/api2/json/access/permissions":
					switch r.URL.Query().Get("path") {
					case "/nodes/pve":
						if tc.name == "missing task access" {
							fmt.Fprint(w, `{"data":{}}`)
						} else {
							fmt.Fprint(w, `{"data":{"/nodes/pve":{"Sys.Audit":0}}}`)
						}
					case "/vms":
						if tc.name == "missing inventory access" {
							fmt.Fprint(w, `{"data":{}}`)
						} else if tc.name == "inventory does not propagate" {
							fmt.Fprint(w, `{"data":{"/vms":{"VM.Audit":0}}}`)
						} else {
							fmt.Fprint(w, `{"data":{"/vms":{"VM.Audit":1}}}`)
						}
					default:
						t.Errorf("unneeded startup permission: %s", r.URL)
						w.WriteHeader(500)
					}
				case "/api2/json/nodes/pve/status":
					if tc.name == "node missing" {
						w.WriteHeader(404)
						fmt.Fprint(w, secret)
					} else if tc.name == "node malformed" {
						fmt.Fprint(w, `{"data":{}}`)
					} else {
						fmt.Fprint(w, `{"data":{"uptime":1234}}`)
					}
				case "/api2/json/cluster/nextid":
					if r.URL.Query().Get("vmid") != "7777" {
						t.Error("wrong owner checked")
					}
					if tc.name == "owner occupied" {
						w.WriteHeader(400)
						fmt.Fprint(w, secret)
					} else {
						fmt.Fprint(w, `{"data":"7777"}`)
					}
				default:
					t.Errorf("unexpected startup operation: %s", r.URL)
					w.WriteHeader(500)
				}
			}))
			defer srv.Close()
			ca := filepath.Join(dir, "ca.pem")
			if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600); err != nil {
				t.Fatal(err)
			}
			values := map[string]string{"PVE_ENDPOINT": srv.URL, "PVE_CA_FILE": ca, "PVE_TOKEN_ID": "test@pve!test", "PVE_TOKEN_SECRET": secret, "PVE_NODE": "pve", "PVE_OWNER_VMID": "7777", "CSI_DRIVER_NAME": "example.csi"}
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			out := &startupOutput{cancel: cancel}
			err = runController(ctx, out, func(k string) string { return values[k] }, "unix://"+socket)
			mu.Lock()
			gotCalls := len(requests)
			mu.Unlock()
			if gotCalls != tc.calls {
				t.Errorf("startup calls=%d want=%d", gotCalls, tc.calls)
			}
			if tc.hint == "" {
				if err != nil || !strings.Contains(out.String(), "CSI listening") {
					t.Fatalf("ready controller did not serve: %v %s", err, out.String())
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.hint) {
					t.Fatalf("startup did not fail early with %q: %v", tc.hint, err)
				}
				if strings.Contains(err.Error(), secret) || strings.Contains(out.String(), secret) {
					t.Fatal("startup leaked credential/response")
				}
				if strings.Contains(out.String(), "CSI listening") {
					t.Fatal("failed startup announced ready")
				}
				if _, err := os.Lstat(socket); !os.IsNotExist(err) {
					t.Fatal("failed startup created socket")
				}
			}
		})
	}
}

func TestInvalidControllerConfigDoesNotContactPVE(t *testing.T) {
	err := runController(context.Background(), io.Discard, func(string) string { return "" }, "unix:///tmp/not-started.sock")
	if err == nil || !strings.Contains(err.Error(), "PVE_ENDPOINT") {
		t.Fatal(err)
	}
}

type healthyIdentity struct {
	csi.UnimplementedIdentityServer
}

func (healthyIdentity) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}

func TestProbeCLIRequiresNoCredentialsOrBuildVersion(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "probe-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "csi.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := grpc.NewServer()
	csi.RegisterIdentityServer(server, healthyIdentity{})
	defer server.Stop()
	go server.Serve(listener)
	getenv := func(key string) string { t.Errorf("probe read environment %s", key); return "" }
	if err := Run(context.Background(), io.Discard, io.Discard, getenv, []string{"probe", "--endpoint=unix://" + path, "--timeout=1s"}); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), io.Discard, io.Discard, getenv, []string{"probe", "--timeout=0"}); err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("bad timeout: %v", err)
	}
}
