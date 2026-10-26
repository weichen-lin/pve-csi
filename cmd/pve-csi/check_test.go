package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"pve-csi.local/csi-plugin/internal/volume"
)

func checkTestEnvironment() map[string]string {
	return map[string]string{
		"PVE_ENDPOINT": "https://pve.example:8006", "PVE_TOKEN_ID": "check@pve!test",
		"PVE_TOKEN_SECRET": "check-secret-must-stay-private", "PVE_NODE": "pve",
		"PVE_OWNER_VMID": "7777", "PVE_STORAGE": "pool", "PVE_VMIDS": " 100, 101 ",
		"PVE_CHECK_TIMEOUT": " 10s ",
	}
}

func TestCheckHelpIgnoresConfiguration(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("PVE_TOKEN_SECRET='invalid-private-value"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			var out, usage strings.Builder
			getenv := func(string) string {
				t.Error("help must not read environment values")
				return "invalid-private-value"
			}
			err := Run(context.Background(), &out, &usage, getenv, []string{"check", arg})
			if !errors.Is(err, flag.ErrHelp) || out.Len() != 0 {
				t.Fatalf("check help: error=%v output=%s", err, out.String())
			}
			for _, key := range []string{"PVE_TOKEN_SECRET", "PVE_VMIDS", "PVE_CHECK_TIMEOUT", "PVE_CHECK_INSECURE"} {
				if !strings.Contains(usage.String(), key) {
					t.Errorf("help omits %s", key)
				}
			}
			for _, unwanted := range []string{"invalid-private-value", "CSI_DRIVER_NAME", "--vmid", "--storage", "--env-file", "--insecure"} {
				if strings.Contains(usage.String(), unwanted) {
					t.Errorf("help contains obsolete or private content: %s", unwanted)
				}
			}
		})
	}
}

func TestCheckRejectsArgumentsBeforeConfiguration(t *testing.T) {
	t.Chdir(t.TempDir())
	const private = "private-argument-do-not-print"
	for _, args := range [][]string{
		{"--vmid", private}, {"--storage=" + private}, {"--env-file", private},
		{"--timeout", private}, {"--insecure"}, {"--insecure=false"}, {private},
		{"--", private}, {"--help", private}, {"-h", private},
	} {
		var firstError string
		for _, malformedFile := range []bool{false, true} {
			if malformedFile {
				if err := os.WriteFile(".env", []byte("PVE_TOKEN_SECRET='private-file-do-not-print"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var out, usage strings.Builder
			getenv := func(string) string {
				t.Error("unsupported arguments must be rejected before reading environment")
				return ""
			}
			err := Run(context.Background(), &out, &usage, getenv, append([]string{"check"}, args...))
			if err == nil || errors.Is(err, flag.ErrHelp) || out.Len() != 0 {
				t.Fatalf("expected argument rejection, error=%v output=%s", err, out.String())
			}
			if strings.Contains(fmt.Sprint(err)+usage.String(), "do-not-print") {
				t.Fatal("argument rejection leaked argument or file contents")
			}
			if !malformedFile {
				firstError = err.Error()
			} else {
				if err.Error() != firstError {
					t.Fatal("argument rejection changed after adding a malformed .env")
				}
				if err := os.Remove(".env"); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func TestCheckRejectsInvalidEnvironmentBeforeNetwork(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tc := range []struct{ name, key, value string }{
		{"missing VMIDs", "PVE_VMIDS", ""},
		{"duplicate VMID", "PVE_VMIDS", "100,100"},
		{"owner selected", "PVE_VMIDS", "7777"},
		{"empty CSV element", "PVE_VMIDS", "100,,101"},
		{"trailing comma", "PVE_VMIDS", "100,"},
		{"space separator", "PVE_VMIDS", "100 101"},
		{"leading zero", "PVE_VMIDS", "0100"},
		{"noninteger", "PVE_VMIDS", "100.1"},
		{"VMID too small", "PVE_VMIDS", "99"},
		{"VMID overflow", "PVE_VMIDS", "99999999999999999999999"},
		{"missing storage", "PVE_STORAGE", ""},
		{"bad storage", "PVE_STORAGE", "../pool"},
		{"missing secret", "PVE_TOKEN_SECRET", ""},
		{"missing node", "PVE_NODE", ""},
		{"bad owner", "PVE_OWNER_VMID", "0100"},
		{"volume handle too long", "PVE_NODE", strings.Repeat("n", 50)},
		{"HTTP endpoint", "PVE_ENDPOINT", "http://pve.example:8006"},
		{"invalid timeout", "PVE_CHECK_TIMEOUT", "invalid-private-value"},
		{"zero timeout", "PVE_CHECK_TIMEOUT", "0s"},
		{"negative timeout", "PVE_CHECK_TIMEOUT", "-1s"},
		{"invalid insecure boolean", "PVE_CHECK_INSECURE", "invalid-private-value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := checkTestEnvironment()
			values[tc.key] = tc.value
			// This would fail client creation if validation did not reject input first.
			values["PVE_CA_FILE"] = filepath.Join(t.TempDir(), "missing.pem")
			var out, usage strings.Builder
			err := Run(context.Background(), &out, &usage, func(k string) string { return values[k] }, []string{"check"})
			if err == nil || strings.Contains(err.Error(), "TLS trust") || out.Len() != 0 {
				t.Fatalf("expected input rejection before client creation; error=%v output=%s", err, out.String())
			}
			if strings.Contains(fmt.Sprint(err)+usage.String(), "invalid-private-value") {
				t.Fatal("input rejection leaked environment contents")
			}
		})
	}
}

func TestCheckCLIWithTLS(t *testing.T) {
	for _, mode := range []string{
		"env only", "dotenv only", "process overrides dotenv",
		"insecure env", "insecure dotenv", "insecure false overrides dotenv",
		"untrusted CA", "invalid CA", "timeout", "unauthenticated", "forbidden", "missing disk grant",
	} {
		t.Run(mode, func(t *testing.T) {
			t.Chdir(t.TempDir())
			values := checkTestEnvironment()
			var requests atomic.Int32
			srv := newCheckTestServer(t, values, mode, &requests)
			defer srv.Close()
			// The displayed target and the Authorization header must use normalized values.
			values["PVE_ENDPOINT"] = " " + srv.URL + "/ "
			values["PVE_TOKEN_ID"] = " check@pve!test "
			values["PVE_CA_FILE"] = filepath.Join(t.TempDir(), "pve-ca.pem")
			caData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
			if mode == "invalid CA" {
				caData = []byte("server-body-private " + values["PVE_TOKEN_SECRET"])
			}
			if err := os.WriteFile(values["PVE_CA_FILE"], caData, 0600); err != nil {
				t.Fatal(err)
			}
			insecure := mode == "insecure env" || mode == "insecure dotenv"
			wantSuccess := mode == "env only" || mode == "dotenv only" || mode == "process overrides dotenv" || insecure
			getenv := func(k string) string { return values[k] }
			switch mode {
			case "untrusted CA", "insecure false overrides dotenv":
				delete(values, "PVE_CA_FILE")
				if mode == "insecure false overrides dotenv" {
					values["PVE_CHECK_INSECURE"] = "false"
					writeCheckDotenv(t, map[string]string{"PVE_CHECK_INSECURE": "true"})
				}
			case "timeout":
				values["PVE_CHECK_TIMEOUT"] = "50ms"
			case "insecure env", "insecure dotenv":
				values["PVE_CHECK_INSECURE"] = "true"
				values["PVE_CA_FILE"] = "missing-ca.pem"
			}
			switch mode {
			case "dotenv only", "insecure dotenv":
				writeCheckDotenv(t, values)
				getenv = func(string) string { return "" }
			case "process overrides dotenv":
				fileValues := checkTestEnvironment()
				fileValues["PVE_CA_FILE"] = values["PVE_CA_FILE"]
				fileValues["PVE_ENDPOINT"] = "https://file-endpoint.invalid:8006"
				fileValues["PVE_TOKEN_ID"] = "file@pve!obsolete"
				fileValues["PVE_TOKEN_SECRET"] = "file-secret-private"
				writeCheckDotenv(t, fileValues)
				getenv = func(k string) string {
					switch k {
					case "PVE_ENDPOINT", "PVE_TOKEN_ID", "PVE_TOKEN_SECRET":
						return values[k]
					default:
						return ""
					}
				}
			}
			var out, usage strings.Builder
			err := Run(context.Background(), &out, &usage, getenv, []string{"check"})
			combined := out.String() + usage.String() + fmt.Sprint(err)
			for _, private := range []string{values["PVE_TOKEN_SECRET"], "server-body-private", "file-secret-private", "file-endpoint.invalid", "file@pve!obsolete"} {
				if strings.Contains(combined, private) {
					t.Fatal("report leaked credentials, server body, or stale target")
				}
			}
			if (err == nil) != wantSuccess {
				t.Fatalf("unexpected result: error=%v output=%s", err, out.String())
			}
			header := fmt.Sprintf("Read-only PVE checks: endpoint=%q token_id=%q\n", srv.URL, strings.TrimSpace(values["PVE_TOKEN_ID"]))
			scope := "Scope: node=\"pve\" storage=\"pool\" owner_vmid=7777 vmids=100,101\n"
			if !strings.HasPrefix(out.String(), header+scope) {
				t.Fatalf("report omits the effective normalized target: %s", out.String())
			}
			if wantSuccess && (!strings.Contains(out.String(), "[PASS]") || !strings.Contains(out.String(), "[WARN]") || !strings.Contains(out.String(), "0 failed")) {
				t.Fatalf("missing success/limitations report: %s", out.String())
			}
			if wantSuccess {
				for _, target := range []string{
					"[PASS] task_permissions /nodes/pve:",
					"[PASS] storage_status pve/pool:",
					"[PASS] reserved_owner /vms/7777:",
					"[PASS] storage_read_permissions /storage/pool:",
					"[WARN] inventory_scope /vms:",
					"[WARN] readonly_scope pve:",
				} {
					if !strings.Contains(out.String(), target) {
						t.Errorf("report omits formatted application target %q", target)
					}
				}
			}
			if strings.Contains(out.String(), "[WARN] tls_verification") != insecure {
				t.Fatalf("TLS warning does not match effective mode: %s", out.String())
			}
			if mode == "missing disk grant" && (!strings.Contains(combined, "VM.Config.Disk") || !strings.Contains(combined, "/vms/101")) {
				t.Fatalf("missing actionable permission failure: %s", combined)
			}
			if mode == "untrusted CA" || mode == "insecure false overrides dotenv" {
				if !strings.Contains(combined, "TLS") || requests.Load() != 0 {
					t.Fatalf("untrusted TLS must fail before sending token; requests=%d output=%s", requests.Load(), combined)
				}
			}
		})
	}
}

func newCheckTestServer(t *testing.T, values map[string]string, mode string, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("check attempted mutation: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "PVEAPIToken="+strings.TrimSpace(values["PVE_TOKEN_ID"])+"="+values["PVE_TOKEN_SECRET"] {
			t.Error("check did not use the effective CSI token")
		}
		if mode == "timeout" {
			<-r.Context().Done()
			return
		}
		if mode == "unauthenticated" || mode == "forbidden" {
			status := http.StatusUnauthorized
			if mode == "forbidden" {
				status = http.StatusForbidden
			}
			w.WriteHeader(status)
			fmt.Fprint(w, "server-body-private "+values["PVE_TOKEN_SECRET"])
			return
		}
		var data any
		switch r.URL.Path {
		case "/api2/json/access/permissions":
			path := r.URL.Query().Get("path")
			grants := map[string]int{}
			switch path {
			case "/nodes/pve":
				grants["Sys.Audit"] = 0
			case "/vms":
				grants["VM.Audit"] = 1
			case "/storage/pool":
				grants = map[string]int{"Datastore.Audit": 0, "Datastore.AllocateSpace": 0, "Datastore.Allocate": 0}
			case "/vms/100", "/vms/101":
				grants["VM.Audit"] = 0
				if mode != "missing disk grant" || path != "/vms/101" {
					grants["VM.Config.Disk"] = 0
				}
			default:
				t.Errorf("unexpected permissions path: %s", path)
			}
			data = map[string]any{path: grants}
		case "/api2/json/nodes":
			data = []map[string]string{{"node": "pve"}}
		case "/api2/json/nodes/pve/status":
			data = map[string]int{"uptime": 1234}
		case "/api2/json/nodes/pve/tasks", "/api2/json/nodes/pve/storage/pool/content", "/api2/json/nodes/pve/qemu/100/pending", "/api2/json/nodes/pve/qemu/101/pending":
			data = []any{}
		case "/api2/json/cluster/nextid":
			data = "7777"
		case "/api2/json/nodes/pve/storage/pool/status":
			data = map[string]any{"type": "lvmthin", "content": "images", "enabled": 1, "active": 1, "avail": int64(10 << 30)}
		case "/api2/json/nodes/pve/qemu":
			data = []map[string]int{{"vmid": 100}, {"vmid": 101}}
		case "/api2/json/nodes/pve/qemu/100/config", "/api2/json/nodes/pve/qemu/101/config":
			uuid := "11111111-1111-4111-8111-111111111111"
			if strings.Contains(r.URL.Path, "/101/") {
				uuid = "22222222-2222-4222-8222-222222222222"
			}
			data = map[string]any{"digest": "digest", "smbios1": "uuid=" + uuid, "scsihw": "virtio-scsi-pci", "hotplug": "disk"}
		default:
			t.Errorf("unexpected check request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	return srv
}

func writeCheckDotenv(t *testing.T, values map[string]string) {
	t.Helper()
	var file strings.Builder
	for key, value := range values {
		fmt.Fprintf(&file, "%s=%q\n", key, value)
	}
	if err := os.WriteFile(".env", []byte(file.String()), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckHeaderEscapesTokenID(t *testing.T) {
	t.Chdir(t.TempDir())
	values := checkTestEnvironment()
	values["PVE_TOKEN_ID"] += "\x1b[31m"
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Exercise reporting without contacting a real endpoint.
	var out, usage strings.Builder
	_ = Run(ctx, &out, &usage, func(k string) string { return values[k] }, []string{"check"})
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), "token_id=\"check@pve!test\\x1b[31m\"") {
		t.Fatal("token ID control bytes must be quoted in the report")
	}
}

func TestCheckEnvFileFailuresBeforeNetwork(t *testing.T) {
	for _, mode := range []string{"malformed", "unreadable"} {
		t.Run(mode, func(t *testing.T) {
			t.Chdir(t.TempDir())
			values := checkTestEnvironment()
			values["PVE_CA_FILE"] = "missing.pem"
			if mode == "malformed" {
				if err := os.WriteFile(".env", []byte("PVE_TOKEN_SECRET='private-value-do-not-print"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(".env", 0700); err != nil {
				t.Fatal(err)
			}
			var out, usage strings.Builder
			err := Run(context.Background(), &out, &usage, func(k string) string { return values[k] }, []string{"check"})
			if err == nil || strings.Contains(err.Error(), "TLS trust") || out.Len() != 0 {
				t.Fatalf("expected file error before client creation: %v, %s", err, out.String())
			}
			if strings.Contains(fmt.Sprint(err)+usage.String(), "do-not-print") {
				t.Fatal("env-file error leaked contents")
			}
		})
	}
}

func TestCheckReportExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []string
		wantErr  bool
	}{
		{"pass with limitations", []string{"PASS", "WARN"}, false},
		{"failure", []string{"PASS", "FAIL"}, true},
		{"skipped required check", []string{"PASS", "SKIP"}, true},
		{"empty", nil, true},
		{"warnings only", []string{"WARN"}, true},
		{"invalid status", []string{"SUCCESS"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var results volume.CheckReport
			for _, status := range tc.statuses {
				results = append(results, volume.CheckResult{
					Name: "test", Target: volume.CheckTarget{Kind: volume.CheckScopeTarget, Node: "pve"},
					Status: status, Detail: "authored hint",
				})
			}
			if err := printCheckResults(io.Discard, results); (err != nil) != tc.wantErr {
				t.Fatalf("result error=%v, want error=%t", err, tc.wantErr)
			}
		})
	}
}
