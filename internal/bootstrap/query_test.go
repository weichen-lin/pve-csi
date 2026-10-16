package bootstrap

import (
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
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

func TestRunAgainstTrustedTLSAPI(t *testing.T) {
	var paths []string
	var pathsMu sync.Mutex
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathsMu.Lock()
		paths = append(paths, r.URL.Path)
		pathsMu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("query mode sent %s, want GET", r.Method)
			http.Error(w, "query mode must be read-only", http.StatusMethodNotAllowed)
			return
		}
		if got := r.Header.Get("Authorization"); got != "PVEAPIToken="+fakeTokenID+"="+fakeSecret {
			t.Errorf("unexpected Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api2/json/version":
			fmt.Fprint(w, `{"data":{"version":"9.1.4","release":"9.1","repoid":"test-repo"}}`)
		case "/api2/json/cluster/resources":
			fmt.Fprint(w, `{"data":[{"id":"qemu/42","type":"qemu","status":"running","name":"test-worker"},{"id":"storage/test/ssd","type":"storage","status":"available","storage":"ssd"}]}`)
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	env := map[string]string{
		"PVE_ENDPOINT": server.URL + "/", "PVE_TOKEN_ID": fakeTokenID,
		"PVE_TOKEN_SECRET": fakeSecret, "PVE_CA_FILE": testCAFile(t, server.Certificate().Raw),
	}
	var out bytes.Buffer
	if err := RunQuery(context.Background(), &out, func(key string) string { return env[key] }); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pve-csi version=", "version=9.1.4", "release=9.1", "repoid=test-repo", "qemu/42", "qemu", "running", "test-worker", "storage/test/ssd", "available"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), fakeSecret) {
		t.Error("output contains token secret")
	}
	pathsMu.Lock()
	defer pathsMu.Unlock()
	if got := strings.Join(paths, ","); got != "/api2/json/version,/api2/json/cluster/resources" {
		t.Errorf("request paths = %q", got)
	}
}

func TestLoadConfigValidation(t *testing.T) {
	base := map[string]string{"PVE_ENDPOINT": "https://pve.example.test:8006/", "PVE_TOKEN_ID": fakeTokenID, "PVE_TOKEN_SECRET": " secret with spaces "}
	cfg, err := LoadConfig(func(key string) string { return base[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Connection.TokenSecret != base["PVE_TOKEN_SECRET"] || cfg.Connection.Endpoint != "https://pve.example.test:8006" {
		t.Fatalf("secret was trimmed or endpoint not normalized")
	}
	base["PVE_ENDPOINT"] += "#"
	cfg, err = LoadConfig(func(key string) string { return base[key] })
	if err != nil || cfg.Connection.Endpoint != "https://pve.example.test:8006" {
		t.Fatalf("empty fragment was not removed from endpoint: %v", err)
	}
	for _, tc := range []struct{ name, key, value string }{
		{"missing endpoint", "PVE_ENDPOINT", ""}, {"missing token ID", "PVE_TOKEN_ID", ""}, {"missing secret", "PVE_TOKEN_SECRET", ""},
		{"HTTP", "PVE_ENDPOINT", "http://pve.example.test"}, {"userinfo", "PVE_ENDPOINT", "https://user@pve.example.test"},
		{"path", "PVE_ENDPOINT", "https://pve.example.test/api2/json"}, {"query", "PVE_ENDPOINT", "https://pve.example.test?x=1"},
		{"fragment", "PVE_ENDPOINT", "https://pve.example.test#x"}, {"missing host", "PVE_ENDPOINT", "https:///"},
		{"secret newline", "PVE_TOKEN_SECRET", "fake\r\nInjected: yes"}, {"ID newline", "PVE_TOKEN_ID", "fake\nInjected: yes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(func(key string) string {
				if key == tc.key {
					return tc.value
				}
				return base[key]
			})
			if err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestRunRejectsIncompleteAPIData(t *testing.T) {
	for _, tc := range []struct{ name, version, resources string }{
		{"missing version", `{}`, `[]`},
		{"blank version", `{"version":" "}`, `[]`},
		{"missing resource ID", `{"version":"9.1.4"}`, `[{"type":"qemu"}]`},
		{"missing resource type", `{"version":"9.1.4"}`, `[{"id":"qemu/42"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data := tc.version
				if r.URL.Path == "/api2/json/cluster/resources" {
					data = tc.resources
				}
				fmt.Fprintf(w, `{"data":%s}`, data)
			}))
			defer server.Close()
			env := map[string]string{"PVE_ENDPOINT": server.URL, "PVE_TOKEN_ID": fakeTokenID, "PVE_TOKEN_SECRET": fakeSecret, "PVE_CA_FILE": testCAFile(t, server.Certificate().Raw)}
			var out bytes.Buffer
			err := RunQuery(context.Background(), &out, func(key string) string { return env[key] })
			if err == nil {
				t.Fatal("incomplete API data accepted")
			}
			if strings.Contains(err.Error()+out.String(), fakeSecret) {
				t.Fatal("secret printed")
			}
		})
	}
}

func TestLoadConfigVMID(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
		valid bool
	}{
		{"", 0, true}, {"100", 100, true}, {"482", 482, true}, {"999999999", 999999999, true},
		{"0", 0, false}, {"99", 0, false}, {"1000000000", 0, false}, {"0100", 0, false},
		{"-100", 0, false}, {"+100", 0, false}, {"100.0", 0, false}, {"1e2", 0, false},
		{"100/../101", 0, false}, {"１２３", 0, false}, {"999999999999999999999", 0, false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			env := map[string]string{"PVE_ENDPOINT": "https://pve.example.test:8006", "PVE_TOKEN_ID": fakeTokenID, "PVE_TOKEN_SECRET": fakeSecret, "PVE_VM_ID": tc.value}
			cfg, err := LoadConfig(func(key string) string { return env[key] })
			if !tc.valid {
				if err == nil || !strings.Contains(err.Error(), "PVE_VM_ID") {
					t.Fatalf("invalid VMID should identify PVE_VM_ID: %v", err)
				}
				return
			}
			if err != nil || cfg.Query.VM.VMID != tc.want {
				t.Fatalf("VMID = %d, error = %v; want %d", cfg.Query.VM.VMID, err, tc.want)
			}
		})
	}
}

func TestRunSelectedVM(t *testing.T) {
	const uuid = "abcdef01-2345-6789-abcd-ef0123456789"
	const hidden = "fake-description-not-for-output"
	var configCalls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "PVEAPIToken="+fakeTokenID+"="+fakeSecret {
			t.Error("unexpected method or Authorization header")
		}
		switch r.URL.Path {
		case "/api2/json/version":
			fmt.Fprint(w, `{"data":{"version":"9.1.4"}}`)
		case "/api2/json/cluster/resources":
			fmt.Fprint(w, `{"data":[{"id":"qemu/482","type":"qemu","vmid":482,"node":"worker-pve","name":"old-name","status":"running"}]}`)
		case "/api2/json/nodes/worker-pve/qemu/482/config":
			configCalls.Add(1)
			if r.URL.RawQuery != "current=1" {
				t.Errorf("VM config query = %q, want current=1", r.URL.RawQuery)
			}
			fmt.Fprintf(w, `{"data":{"name":"test-worker","smbios1":"serial=fake-serial,uuid=%s","description":"%s"}}`, strings.ToUpper(uuid), hidden)
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	env := map[string]string{"PVE_ENDPOINT": server.URL, "PVE_TOKEN_ID": fakeTokenID, "PVE_TOKEN_SECRET": fakeSecret, "PVE_CA_FILE": testCAFile(t, server.Certificate().Raw), "PVE_VM_ID": "482"}
	var out bytes.Buffer
	if err := RunQuery(context.Background(), &out, func(key string) string { return env[key] }); err != nil {
		t.Fatal(err)
	}
	if configCalls.Load() != 1 || !strings.Contains(out.String(), "PVE VM vmid=482 node=worker-pve name=test-worker uuid="+uuid) {
		t.Fatalf("VM config calls = %d, output = %s", configCalls.Load(), out.String())
	}
	for _, forbidden := range []string{fakeSecret, hidden, "fake-serial", "smbios1"} {
		if strings.Contains(out.String(), forbidden) {
			t.Errorf("output exposed %q", forbidden)
		}
	}
}

func TestStorageConfig(t *testing.T) {
	for _, tc := range []struct {
		name, storage, vmID string
		wantErr             bool
	}{
		{"optional", "", "", false}, {"VM only", "", "482", false},
		{"ordinary", "fast_pool", "482", false}, {"trimmed", " data-ssd ", "482", false},
		{"mixed case", "Fast.Pool_2", "482", false}, {"requires VM", "fast_pool", "", true},
		{"leading digit", "1fast", "482", true}, {"leading dash", "-fast", "482", true},
		{"trailing dash", "fast-", "482", true}, {"trailing dot", "fast.", "482", true},
		{"trailing underscore", "fast_", "482", true}, {"path", "fast/pool", "482", true},
		{"query", "fast?pool", "482", true}, {"space", "fast pool", "482", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{
				"PVE_ENDPOINT": "https://pve.example.test:8006", "PVE_TOKEN_ID": fakeTokenID,
				"PVE_TOKEN_SECRET": fakeSecret, "PVE_VM_ID": tc.vmID, "PVE_STORAGE": tc.storage,
			}
			cfg, err := LoadConfig(func(key string) string { return env[key] })
			if (err != nil) != tc.wantErr {
				t.Fatalf("loadConfig error = %v, want error %t", err, tc.wantErr)
			}
			if err == nil && cfg.Query.Storage != strings.TrimSpace(tc.storage) {
				t.Errorf("storage ID = %q", cfg.Query.Storage)
			}
		})
	}
}

func TestRunQueriesStorageOnSelectedVMNode(t *testing.T) {
	var paths []string
	var mu sync.Mutex
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "PVEAPIToken="+fakeTokenID+"="+fakeSecret {
			t.Error("unexpected method or Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.RequestURI() {
		case "/api2/json/version":
			fmt.Fprint(w, `{"data":{"version":"9.1.4"}}`)
		case "/api2/json/cluster/resources":
			fmt.Fprint(w, `{"data":[{"id":"qemu/482","type":"qemu","vmid":482,"node":"worker-pve","name":"worker-b"}]}`)
		case "/api2/json/nodes/worker-pve/qemu/482/config?current=1":
			fmt.Fprint(w, `{"data":{"name":"worker-b","smbios1":"uuid=aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}}`)
		case "/api2/json/nodes/worker-pve/storage/fast_pool/status":
			fmt.Fprint(w, `{"data":{"type":"lvmthin","content":"images,rootdir","enabled":1,"active":1,"total":17179869184,"used":4294967296,"avail":10737418240}}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.RequestURI())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	env := map[string]string{
		"PVE_ENDPOINT": server.URL, "PVE_TOKEN_ID": fakeTokenID, "PVE_TOKEN_SECRET": fakeSecret,
		"PVE_CA_FILE": testCAFile(t, server.Certificate().Raw), "PVE_VM_ID": "482", "PVE_STORAGE": "fast_pool",
	}
	var out bytes.Buffer
	if err := RunQuery(context.Background(), &out, func(key string) string { return env[key] }); err != nil {
		t.Fatal(err)
	}
	want := "PVE storage=fast_pool node=worker-pve type=lvmthin enabled=true active=true total=16.00GiB used=4.00GiB available=10.00GiB content=images,rootdir\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("missing storage summary: %s", out.String())
	}
	if strings.Contains(out.String(), fakeSecret) {
		t.Error("output contains token secret")
	}
	mu.Lock()
	defer mu.Unlock()
	wantPaths := "/api2/json/version,/api2/json/cluster/resources,/api2/json/nodes/worker-pve/qemu/482/config?current=1,/api2/json/nodes/worker-pve/storage/fast_pool/status"
	if got := strings.Join(paths, ","); got != wantPaths {
		t.Errorf("request paths = %q", got)
	}
}

func TestIdentityGuardConfig(t *testing.T) {
	const uuid = "abcdef01-2345-6789-abcd-ef0123456789"
	for _, tc := range []struct {
		name, vmID, uuid, vmName string
		wantUUID, wantName       string
		wantError                string
	}{
		{name: "optional"},
		{name: "blank guards", uuid: " \t ", vmName: " \t "},
		{name: "uppercase UUID", vmID: "482", uuid: " " + strings.ToUpper(uuid) + " ", wantUUID: uuid},
		{name: "name only", vmID: "482", vmName: " pve-worker-b ", wantName: "pve-worker-b"},
		{name: "both guards", vmID: "482", uuid: uuid, vmName: "pve-worker-b", wantUUID: uuid, wantName: "pve-worker-b"},
		{name: "UUID requires VM", uuid: uuid, wantError: "PVE_VM_ID"},
		{name: "name requires VM", vmName: "pve-worker-b", wantError: "PVE_VM_ID"},
		{name: "invalid UUID", vmID: "482", uuid: "not-a-uuid", wantError: "PVE_EXPECTED_VM_UUID"},
		{name: "UUID without hyphens", vmID: "482", uuid: "abcdef0123456789abcdef0123456789", wantError: "PVE_EXPECTED_VM_UUID"},
		{name: "UUID with nonhex digit", vmID: "482", uuid: "zbcdef01-2345-6789-abcd-ef0123456789", wantError: "PVE_EXPECTED_VM_UUID"},
		{name: "name newline", vmID: "482", vmName: "pve\nworker", wantError: "PVE_EXPECTED_VM_NAME"},
		{name: "name carriage return", vmID: "482", vmName: "pve\rworker", wantError: "PVE_EXPECTED_VM_NAME"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{
				"PVE_ENDPOINT": "https://pve.example.test:8006", "PVE_TOKEN_ID": fakeTokenID,
				"PVE_TOKEN_SECRET": fakeSecret, "PVE_VM_ID": tc.vmID,
				"PVE_EXPECTED_VM_UUID": tc.uuid, "PVE_EXPECTED_VM_NAME": tc.vmName,
			}
			cfg, err := LoadConfig(func(key string) string { return env[key] })
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("loadConfig error = %v, want %s", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Query.VM.ExpectedUUID != tc.wantUUID || cfg.Query.VM.ExpectedName != tc.wantName {
				t.Errorf("guards = UUID %q, name %q; want %q, %q", cfg.Query.VM.ExpectedUUID, cfg.Query.VM.ExpectedName, tc.wantUUID, tc.wantName)
			}
		})
	}
}

func TestRunIdentityGuardBeforeStorage(t *testing.T) {
	const uuid = "abcdef01-2345-6789-abcd-ef0123456789"
	for _, tc := range []struct {
		name, expectedUUID, expectedName string
		configName, resourceName         string
		wantChecks, wantError            string
	}{
		{name: "UUID and name match", expectedUUID: strings.ToUpper(uuid), expectedName: "pve-worker-b", configName: "pve-worker-b", resourceName: "stale-name", wantChecks: "uuid,name"},
		{name: "UUID only matches", expectedUUID: uuid, configName: "pve-worker-b", wantChecks: "uuid"},
		{name: "name only matches", expectedName: "pve-worker-b", configName: "pve-worker-b", wantChecks: "name"},
		{name: "stale resource name cannot verify current name", expectedName: "pve-worker-b", resourceName: "pve-worker-b", wantError: "name mismatch"},
		{name: "UUID only permits display name fallback", expectedUUID: uuid, resourceName: "pve-worker-b", wantChecks: "uuid"},
		{name: "no guards", configName: "pve-worker-b"},
		{name: "UUID mismatch", expectedUUID: "11111111-2222-3333-4444-555555555555", expectedName: "pve-worker-b", configName: "pve-worker-b", wantError: "UUID mismatch"},
		{name: "name mismatch", expectedUUID: uuid, expectedName: "other-vm", configName: "pve-worker-b", wantError: "name mismatch"},
		{name: "fallback name mismatch", expectedName: "other-vm", resourceName: "pve-worker-b", wantError: "name mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var storageCalls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "PVEAPIToken="+fakeTokenID+"="+fakeSecret {
					t.Error("unexpected method or Authorization header")
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.RequestURI() {
				case "/api2/json/version":
					fmt.Fprint(w, `{"data":{"version":"9.1.4"}}`)
				case "/api2/json/cluster/resources":
					fmt.Fprintf(w, `{"data":[{"id":"qemu/482","type":"qemu","vmid":482,"node":"worker-pve","name":%q}]}`, tc.resourceName)
				case "/api2/json/nodes/worker-pve/qemu/482/config?current=1":
					fmt.Fprintf(w, `{"data":{"name":%q,"smbios1":"uuid=%s"}}`, tc.configName, strings.ToUpper(uuid))
				case "/api2/json/nodes/worker-pve/storage/fast_pool/status":
					storageCalls.Add(1)
					fmt.Fprint(w, `{"data":{"type":"lvmthin","content":"images","enabled":1,"active":1,"total":100,"used":10,"avail":90}}`)
				default:
					t.Errorf("unexpected request: %s", r.URL.RequestURI())
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			env := map[string]string{
				"PVE_ENDPOINT": server.URL, "PVE_TOKEN_ID": fakeTokenID, "PVE_TOKEN_SECRET": fakeSecret,
				"PVE_CA_FILE": testCAFile(t, server.Certificate().Raw), "PVE_VM_ID": "482", "PVE_STORAGE": "fast_pool",
				"PVE_EXPECTED_VM_UUID": tc.expectedUUID, "PVE_EXPECTED_VM_NAME": tc.expectedName,
			}
			var out bytes.Buffer
			err := RunQuery(context.Background(), &out, func(key string) string { return env[key] })
			if strings.Contains(out.String(), fakeSecret) || (err != nil && strings.Contains(err.Error(), fakeSecret)) {
				t.Fatal("identity check exposed token secret")
			}
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("run error = %v, want %s", err, tc.wantError)
				}
				if storageCalls.Load() != 0 {
					t.Error("storage queried after VM identity mismatch")
				}
				if strings.Contains(out.String(), "identity verified") || strings.Contains(out.String(), "PVE VM vmid=") {
					t.Errorf("identity mismatch printed success: %s", out.String())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if storageCalls.Load() != 1 {
				t.Errorf("storage calls = %d, want 1", storageCalls.Load())
			}
			if !strings.Contains(out.String(), "uuid="+uuid) {
				t.Errorf("missing normalized VM UUID: %s", out.String())
			}
			if tc.wantChecks == "" {
				if strings.Contains(out.String(), "identity verified") {
					t.Errorf("unguarded query claimed identity verification: %s", out.String())
				}
				return
			}
			want := "PVE VM identity verified vmid=482 checks=" + tc.wantChecks + "\n"
			verifiedAt := strings.Index(out.String(), want)
			storageAt := strings.Index(out.String(), "PVE storage=")
			if verifiedAt < 0 || storageAt <= verifiedAt {
				t.Errorf("expected verification before storage summary: %s", out.String())
			}
		})
	}
}
