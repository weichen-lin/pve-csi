package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

func TestTaskCompletionValidation(t *testing.T) {
	for _, operation := range []string{"attach", "delete"} {
		kind, target := "qmconfig", "100"
		if operation == "delete" {
			kind, target = "imgdel", "7777@pool"
		}
		upid := fmt.Sprintf("UPID:pve:1:1:1:%s:%s:test:", kind, target)
		for _, tc := range []struct {
			name, returned  string
			fields          map[string]string
			httpStatus      int
			running, cancel bool
			want            string
		}{
			{name: "success", want: "success"},
			{name: "running then success", running: true, want: "success"},
			{name: "failed", fields: map[string]string{"exitstatus": "ERROR"}, want: "failed"},
			{name: "cancelled", cancel: true, want: "unknown"},
			{name: "HTTP error", httpStatus: http.StatusServiceUnavailable, want: "unknown"},
			{name: "wrong UPID", fields: map[string]string{"upid": "UPID:pve:2:2:2:" + kind + ":" + target + ":test:"}, want: "unknown"},
			{name: "wrong type", fields: map[string]string{"type": "other"}, want: "unknown"},
			{name: "wrong target", fields: map[string]string{"id": "999"}, want: "unknown"},
			{name: "missing exit status", fields: map[string]string{"exitstatus": ""}, want: "unknown"},
			{name: "invalid status", fields: map[string]string{"status": "unknown"}, want: "unknown"},
			{name: "invalid UPID node", returned: strings.Replace(upid, ":pve:", ":other:", 1), want: "unknown"},
			{name: "invalid UPID type", returned: strings.Replace(upid, ":"+kind+":", ":other:", 1), want: "unknown"},
			{name: "invalid UPID target", returned: strings.Replace(upid, ":"+target+":", ":999:", 1), want: "unknown"},
			{name: "missing UPID user", returned: strings.Replace(upid, ":test:", "::", 1), want: "unknown"},
			{name: "invalid UPID terminator", returned: upid + "unexpected", want: "unknown"},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				returned := tc.returned
				if returned == "" {
					returned = upid
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				reads, writes := 0, 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost || r.Method == http.MethodDelete {
						writes++
						_ = json.NewEncoder(w).Encode(map[string]any{"data": returned})
						return
					}
					reads++
					if r.Method != http.MethodGet || r.URL.Path != "/api2/json/nodes/pve/tasks/"+returned+"/status" {
						t.Errorf("unexpected task request: %s %s", r.Method, r.URL)
					}
					if tc.httpStatus != 0 {
						w.WriteHeader(tc.httpStatus)
						return
					}
					state := map[string]string{"upid": returned, "type": kind, "id": target, "status": "stopped", "exitstatus": "OK"}
					for field, value := range tc.fields {
						state[field] = value
						if value == "" {
							delete(state, field)
						}
					}
					if tc.running && reads == 1 || tc.cancel {
						state["status"] = "running"
						delete(state, "exitstatus")
					}
					if tc.cancel {
						cancel()
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": state})
				}))
				defer srv.Close()
				client := &Client{httpClient: srv.Client(), config: Config{Endpoint: srv.URL}}
				unknown := volume.ErrAttachmentUnconfirmed
				var err error
				if operation == "attach" {
					g, _ := NewAttachmentGateway(client)
					err = g.update(ctx, "/api2/json/nodes/pve/qemu/100", domain.VirtualMachine{Node: "pve", VMID: 100}, url.Values{})
				} else {
					g, _ := NewStorageGateway(client)
					req, _ := domain.NewRequest("task", domain.Placement{Node: "pve", Storage: "pool"}, 7777, domain.GiB, 0)
					id, _ := domain.ParseHandle(req.Handle())
					err = g.Delete(ctx, id)
					unknown = volume.ErrDeleteUnconfirmed
				}
				switch tc.want {
				case "success":
					if err != nil {
						t.Fatalf("completion: %v", err)
					}
				case "failed":
					var backend *volume.BackendError
					failed := errors.Is(err, volume.ErrDeleteFailed)
					if operation == "attach" {
						failed = errors.As(err, &backend) && backend.Kind() == volume.FailurePrecondition
					}
					if !failed || errors.Is(err, unknown) {
						t.Fatalf("lost task failure: %v", err)
					}
				case "unknown":
					if !errors.Is(err, unknown) {
						t.Fatalf("unconfirmed result accepted: %v", err)
					}
				default:
					t.Fatalf("invalid expected result %q", tc.want)
				}
				if tc.cancel && !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
				wantReads := 1
				if tc.running {
					wantReads = 2
				}
				if returned != upid {
					wantReads = 0
				}
				if writes != 1 || reads != wantReads {
					t.Fatalf("requests: writes=%d reads=%d; want 1/%d", writes, reads, wantReads)
				}
			})
		}
	}
}

func TestTaskVisibilityBeforeIdleDecisions(t *testing.T) {
	for _, operation := range []string{"delete", "references", "attach", "detach"} {
		for _, access := range []string{"missing", "revoked during list", "granted", "old token task"} {
			t.Run(operation+"/"+access, func(t *testing.T) {
				checks, reads, writes := 0, 0, 0
				req, err := domain.NewRequest("safe", domain.Placement{Node: "pve", Storage: "pool"}, 7777, domain.GiB, 0)
				if err != nil {
					t.Fatal(err)
				}
				id, _ := domain.ParseHandle(req.Handle())
				uuid, _ := domain.ParseNodeID("153a7c9e-588b-483f-884a-4eba01bc160a")
				vm := domain.VirtualMachine{Node: "pve", VMID: 100, ID: uuid}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "GET" {
						writes++
						t.Error("must not write in visibility test")
					}
					var data any = []any{}
					switch {
					case strings.HasSuffix(r.URL.Path, "/permissions"):
						path := r.URL.Query().Get("path")
						grants := map[string]int{"VM.Audit": 1, "VM.Config.Disk": 0}
						if path == "/nodes/pve" {
							checks++
							grants = map[string]int{}
							if access != "missing" && !(access == "revoked during list" && checks > 1) {
								grants["Sys.Audit"] = 0
							}
						}
						data = map[string]any{path: grants}
					case strings.HasSuffix(r.URL.Path, "/tasks"):
						reads++
						if access == "old token task" {
							taskID := "100"
							if operation == "delete" {
								taskID = "7777@pool"
							}
							data = []any{map[string]string{"type": r.URL.Query().Get("typefilter"), "id": taskID, "status": "RUNNING", "user": "pve-csi@pve", "tokenid": "old-token"}}
						}
					case strings.HasSuffix(r.URL.Path, "/config"):
						// A later, independent guard prevents writes for the granted case.
						data = map[string]string{"digest": "abc", "smbios1": "uuid=" + uuid.String(), "lock": "backup"}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
				}))
				defer srv.Close()
				client := &Client{httpClient: srv.Client(), config: Config{Endpoint: srv.URL}}
				g, _ := NewAttachmentGateway(client)
				switch operation {
				case "delete":
					storage, _ := NewStorageGateway(client)
					err = storage.CheckIdle(context.Background(), id.Placement(), id.OwnerVMID())
				case "references":
					_, err = g.References(context.Background(), id)
				case "attach":
					err = g.Attach(context.Background(), id, vm)
				case "detach":
					err = g.Detach(context.Background(), id, domain.DiskReference{Source: vm.ReferenceSource(), Slot: "scsi1", Serial: id.Serial(), State: domain.ReferenceAttached})
				}
				if access == "missing" || access == "revoked during list" {
					var backend *volume.BackendError
					if !errors.As(err, &backend) || backend.Kind() != volume.FailurePermissionDenied || !strings.Contains(backend.Hint(), "Sys.Audit on /nodes/pve") {
						t.Fatalf("permission loss treated as idle: %v", err)
					}
					if access == "missing" && reads != 0 {
						t.Fatal("queried filtered list")
					}
				} else {
					if checks < 2 || reads != 1 {
						t.Fatalf("unchecked observation: checks=%d reads=%d", checks, reads)
					}
					wantBusy := access == "old token task" || operation == "attach" || operation == "detach"
					if (wantBusy && !errors.Is(err, volume.ErrBusy)) || (!wantBusy && err != nil) {
						t.Fatalf("idle result: %v", err)
					}
				}
				if writes != 0 {
					t.Fatal("unexpected mutation")
				}
			})
		}
	}
}
