package pveapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFindVM(t *testing.T) {
	vm := Resource{ID: "qemu/482", Type: "qemu", VMID: 482, Node: "worker-pve", Name: "test-worker", Status: "stopped"}
	if got, err := FindVM([]Resource{{ID: "qemu/483", Type: "qemu", VMID: 483}, vm}, 482); err != nil || got != vm {
		t.Fatalf("stopped VM lookup = %#v, %v", got, err)
	}
	for _, tc := range []struct {
		name    string
		entries []Resource
	}{
		{"missing", nil},
		{"container only", []Resource{{ID: "lxc/482", Type: "lxc", VMID: 482, Node: "worker-pve"}}},
		{"duplicate", []Resource{vm, vm}},
		{"no node", []Resource{{ID: vm.ID, Type: vm.Type, VMID: vm.VMID}}},
		{"blank node", []Resource{{ID: vm.ID, Type: vm.Type, VMID: vm.VMID, Node: " "}}},
		{"inconsistent ID", []Resource{{ID: "qemu/483", Type: vm.Type, VMID: vm.VMID, Node: vm.Node}}},
		{"template", []Resource{{ID: vm.ID, Type: vm.Type, VMID: vm.VMID, Node: vm.Node, Template: 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FindVM(tc.entries, 482)
			if err == nil {
				t.Fatal("invalid VM selection accepted")
			}
			if tc.name == "missing" && !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "not visible") {
				t.Fatalf("missing VM error is unclear: %v", err)
			}
		})
	}
}

func TestSMBIOSUUID(t *testing.T) {
	const uuid = "abcdef01-2345-6789-abcd-ef0123456789"
	for _, tc := range []struct{ raw, want string }{
		{"uuid=" + uuid, uuid},
		{"serial=test-serial,uuid=" + strings.ToUpper(uuid) + ",manufacturer=test", uuid},
		{"uuid=00000000-0000-0000-0000-000000000000", "00000000-0000-0000-0000-000000000000"},
	} {
		if got, err := SMBIOSUUID(tc.raw); err != nil || got != tc.want {
			t.Errorf("SMBIOSUUID(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
	for _, raw := range []string{
		"", "serial=test", "uuid=", "serial=uuid=" + uuid, "UUID=" + uuid,
		"uuid=" + uuid + ",uuid=" + uuid, "uuid=,uuid=" + uuid,
		"uuid=abcdef0123456789abcdef0123456789", "uuid=ghijkl01-2345-6789-abcd-ef0123456789",
		"uuid=abcdef01-2345-6789-abcd-ef012345678", "uuid=" + uuid + "extra",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := SMBIOSUUID(raw); err == nil {
				t.Fatal("missing, ambiguous, or malformed UUID accepted")
			}
		})
	}
}

func TestQueryVMConfig(t *testing.T) {
	const uuid = "abcdef01-2345-6789-abcd-ef0123456789"
	for _, tc := range []struct {
		name   string
		status int
		body   string
		valid  bool
	}{
		{"name fallback", 200, `{"data":{"smbios1":"uuid=` + uuid + `"}}`, true},
		{"permission denied", 403, fakeSecret, false},
		{"missing UUID", 200, `{"data":{"name":"test-worker","smbios1":"serial=fake"}}`, false},
		{"template config", 200, `{"data":{"template":1,"smbios1":"uuid=` + uuid + `"}}`, false},
		{"invalid config shape", 200, `{"data":[]}`, false},
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
			defer client.CloseIdleConnections()
			cfg := testConfig(server.URL)
			api := &Client{httpClient: client, config: cfg}
			info, err := api.QueryVM(context.Background(), VMQuery{VMID: 482}, []Resource{{ID: "qemu/482", Type: "qemu", VMID: 482, Node: "worker-pve", Name: "fallback-worker"}})
			if tc.valid {
				if err != nil || info.VMID != 482 || info.Node != "worker-pve" || info.Name != "fallback-worker" || info.UUID != uuid {
					t.Fatalf("VM info = %#v, error = %v", info, err)
				}
			} else if err == nil {
				t.Fatal("invalid VM config accepted")
			}
			if err != nil && strings.Contains(err.Error(), fakeSecret) {
				t.Fatal("error exposed token secret")
			}
		})
	}
}
