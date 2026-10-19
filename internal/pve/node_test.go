package pve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

func TestResolveNode(t *testing.T) {
	const uuid = "153a7c9e-588b-483f-884a-4eba01bc160a"
	const other = "38f7a2bf-c484-47c9-90f6-250caaf28986"
	const listPath = "/api2/json/nodes/pve/qemu"
	const configPath = listPath + "/101/config"
	for _, tt := range []struct {
		name   string
		path   string
		body   string
		status int
		want   error
		kind   volume.FailureKind
		step   string
	}{
		{name: "unique including template"},
		{name: "missing", path: listPath, body: `{"data":[]}`, want: domain.ErrNodeNotFound, step: "resolve_node"},
		{name: "duplicate UUID", path: configPath, body: `{"data":{"smbios1":"uuid=` + uuid + `"}}`, want: domain.ErrNodeAmbiguous, step: "resolve_node"},
		{name: "no audit", path: "/vms", body: `{"data":{}}`, kind: volume.FailurePermissionDenied, step: "check_vm_permissions"},
		{name: "audit does not propagate", path: "/vms", body: `{"data":{"/vms":{"VM.Audit":0}}}`, kind: volume.FailurePermissionDenied, step: "check_vm_permissions"},
		{name: "child denied", path: "/vms/101", body: `{"data":{}}`, kind: volume.FailurePermissionDenied, step: "check_vm_permissions"},
		{name: "401", path: listPath, status: 401, kind: volume.FailureUnauthenticated, step: "list_vms"},
		{name: "403 after match", path: configPath, status: 403, kind: volume.FailurePermissionDenied, step: "read_vm_identity"},
		{name: "500 after match", path: configPath, status: 500, kind: volume.FailureUnavailable, step: "read_vm_identity"},
		{name: "bad config", path: configPath, body: `{"data":{}}`, kind: volume.FailureInternal, step: "read_vm_identity"},
		{name: "duplicate UUID field", path: configPath, body: `{"data":{"smbios1":"uuid=` + other + `,uuid=` + uuid + `"}}`, kind: volume.FailureInternal, step: "read_vm_identity"},
		{name: "malformed UUID", path: configPath, body: `{"data":{"smbios1":"uuid=bad"}}`, kind: volume.FailureInternal, step: "read_vm_identity"},
		{name: "became template", path: configPath, body: `{"data":{"template":1,"smbios1":"uuid=` + other + `"}}`, kind: volume.FailureInternal, step: "read_vm_identity"},
		{name: "null list", path: listPath, body: `{"data":null}`, kind: volume.FailureInternal, step: "list_vms"},
		{name: "duplicate VMID", path: listPath, body: `{"data":[{"vmid":100},{"vmid":100}]}`, kind: volume.FailureInternal, step: "list_vms"},
		{name: "invalid VMID", path: listPath, body: `{"data":[{}]}`, kind: volume.FailureInternal, step: "list_vms"},
		{name: "invalid template flag", path: listPath, body: `{"data":[{"vmid":100,"template":2}]}`, kind: volume.FailureInternal, step: "list_vms"},
		{name: "revoked after scan", kind: volume.FailurePermissionDenied, step: "check_vm_permissions"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rootChecks := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected mutation: %s", r.Method)
				}
				path := r.URL.Path
				if path == "/api2/json/access/permissions" {
					path = r.URL.Query().Get("path")
					if path == "/vms" {
						rootChecks++
						if tt.name == "revoked after scan" && rootChecks > 1 {
							io.WriteString(w, `{"data":{}}`)
							return
						}
					}
				}
				if path == tt.path {
					if tt.status != 0 {
						w.WriteHeader(tt.status)
					}
					io.WriteString(w, tt.body)
					return
				}
				switch path {
				case "/vms", "/vms/100", "/vms/101":
					propagate := 0 // An exact-path grant with value 0 still permits reading.
					if path == "/vms" {
						propagate = 1
					}
					fmt.Fprintf(w, `{"data":{%q:{"VM.Audit":%d}}}`, path, propagate)
				case listPath:
					io.WriteString(w, `{"data":[{"vmid":100},{"vmid":101},{"vmid":9000,"template":1}]}`)
				case listPath + "/100/config", configPath:
					if r.URL.Query().Get("current") != "1" {
						t.Error("must read current config, not pending changes")
					}
					id := uuid
					if path == configPath {
						id = other
					}
					fmt.Fprintf(w, `{"data":{"smbios1":"serial=abc,uuid=%s,base64=1"}}`, id)
				default:
					t.Errorf("unexpected request: %s", r.URL)
					w.WriteHeader(500)
				}
			}))
			t.Cleanup(server.Close)
			client := &Client{httpClient: server.Client(), config: Config{Endpoint: server.URL}}
			inventory, err := NewNodeInventory(client)
			if err != nil {
				t.Fatal(err)
			}
			resolver, err := volume.NewNodeResolver(inventory, "pve")
			if err != nil {
				t.Fatal(err)
			}
			vm, err := resolver.Resolve(context.Background(), uuid)
			if tt.step == "" {
				if err != nil || vm.VMID != 100 || vm.ID.String() != uuid || rootChecks != 2 {
					t.Fatalf("Resolve = %v, %v; root checks %d", vm, err, rootChecks)
				}
				return
			}
			if err == nil || vm != (domain.VirtualMachine{}) {
				t.Fatalf("partial success: %v, %v", vm, err)
			}
			var step *volume.StepError
			if !errors.As(err, &step) || step.Step() != tt.step {
				t.Fatalf("step = %v, want %s", err, tt.step)
			}
			if tt.want != nil {
				if !errors.Is(err, tt.want) {
					t.Fatalf("error = %v, want %v", err, tt.want)
				}
			} else {
				var backend *volume.BackendError
				if !errors.As(err, &backend) || backend.Kind() != tt.kind {
					t.Fatalf("backend = %+v, want %v", backend, tt.kind)
				}
			}
		})
	}
}
