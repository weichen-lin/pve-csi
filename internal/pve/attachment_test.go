package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

func TestReferenceScanKeepsGuestResultsSeparateAndRejectsPartialScans(t *testing.T) {
	req, err := domain.NewRequest("scan", domain.Placement{Node: "pve", Storage: "pool"}, 7777, domain.GiB, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := domain.ParseHandle(req.Handle())
	const uuid100 = "153a7c9e-588b-483f-884a-4eba01bc160a"
	const uuid101 = "38f7a2bf-c484-47c9-90f6-250caaf28986"
	for _, scenario := range []string{"complete", "later guest fails", "access revoked", "guest denied", "invalid qemu template", "invalid lxc template", "duplicate VMID", "invalid VMID"} {
		t.Run(scenario, func(t *testing.T) {
			rootChecks := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("reference scan attempted mutation")
				}
				var data any = []any{}
				switch r.URL.Path {
				case "/api2/json/access/permissions":
					path := r.URL.Query().Get("path")
					grants := map[string]int{"VM.Audit": 1, "Sys.Audit": 0}
					if path == "/vms" {
						rootChecks++
						if scenario == "access revoked" && rootChecks > 1 {
							grants = map[string]int{}
						}
					}
					if path == "/vms/101" && scenario == "guest denied" {
						grants = map[string]int{}
					}
					data = map[string]any{path: grants}
				case "/api2/json/nodes/pve/qemu":
					data = []any{map[string]int{"vmid": 100}, map[string]int{"vmid": 101}}
					if scenario == "invalid qemu template" {
						data = []any{map[string]int{"vmid": 100, "template": 2}}
					}
				case "/api2/json/nodes/pve/lxc":
					data = []any{map[string]int{"vmid": 200}}
					switch scenario {
					case "invalid lxc template":
						data = []any{map[string]int{"vmid": 200, "template": 2}}
					case "duplicate VMID":
						data = []any{map[string]int{"vmid": 200}, map[string]int{"vmid": 200}}
					case "invalid VMID":
						data = []any{map[string]int{"vmid": 99}}
					}
				case "/api2/json/nodes/pve/qemu/100/config":
					data = map[string]string{"digest": "a", "smbios1": "uuid=" + uuid100, "scsi1": id.Reference() + ",serial=" + id.Serial()}
				case "/api2/json/nodes/pve/qemu/101/config":
					if scenario == "guest denied" {
						t.Error("read guest configuration without permission")
					}
					data = map[string]string{"digest": "b", "smbios1": "uuid=" + uuid101, "scsi1": id.Reference() + ",serial=" + id.Serial()}
				case "/api2/json/nodes/pve/qemu/101/pending":
					if scenario == "later guest fails" {
						w.WriteHeader(500)
						return
					}
					data = []any{map[string]any{"key": "scsi1", "delete": 1}, map[string]any{"key": "scsi2", "pending": id.Reference() + ",serial=" + id.Serial()}}
				case "/api2/json/nodes/pve/qemu/100/snapshot":
					data = []any{map[string]string{"name": "current"}, map[string]string{"name": "saved"}}
				case "/api2/json/nodes/pve/qemu/100/snapshot/saved/config":
					data = map[string]string{"scsi1": id.Reference() + ",serial=" + id.Serial()}
				case "/api2/json/nodes/pve/lxc/200/config":
					data = map[string]string{"digest": "c", "rootfs": id.Reference()}
				case "/api2/json/nodes/pve/tasks", "/api2/json/nodes/pve/qemu/100/pending", "/api2/json/nodes/pve/qemu/101/snapshot", "/api2/json/nodes/pve/lxc/200/pending", "/api2/json/nodes/pve/lxc/200/snapshot":
				default:
					t.Errorf("unexpected path: %s", r.URL)
					w.WriteHeader(500)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))
			defer srv.Close()
			g, err := NewAttachmentGateway(&Client{httpClient: srv.Client(), config: Config{Endpoint: srv.URL}})
			if err != nil {
				t.Fatal(err)
			}
			refs, err := g.References(context.Background(), id)
			if strings.HasPrefix(scenario, "invalid ") || scenario == "duplicate VMID" {
				if refs != nil || !errors.Is(err, domain.ErrInvalidInventory) {
					t.Fatalf("invalid inventory accepted: %+v %v", refs, err)
				}
				return
			}
			if scenario != "complete" {
				var backend *volume.BackendError
				want := volume.FailureUnavailable
				if scenario == "access revoked" || scenario == "guest denied" {
					want = volume.FailurePermissionDenied
				}
				if refs != nil || !errors.As(err, &backend) || backend.Kind() != want {
					t.Fatalf("partial scan accepted: %+v %v", refs, err)
				}
				return
			}
			if err != nil || len(refs) != 5 || rootChecks != 2 {
				t.Fatalf("scan=%+v error=%v checks=%d", refs, err, rootChecks)
			}
			want := map[string]string{
				fmt.Sprintf("100/scsi1/%d", domain.ReferenceAttached):  uuid100,
				fmt.Sprintf("100/scsi1/%d", domain.ReferenceSnapshot):  uuid100,
				fmt.Sprintf("101/scsi1/%d", domain.ReferencePending):   uuid101,
				fmt.Sprintf("101/scsi2/%d", domain.ReferencePending):   uuid101,
				fmt.Sprintf("200/rootfs/%d", domain.ReferenceAttached): "",
			}
			for _, ref := range refs {
				key := fmt.Sprintf("%d/%s/%d", ref.Source.VMID, ref.Slot, ref.State)
				uuid, found := want[key]
				if !found || ref.Source.NodeID.String() != uuid || ref.Source.Node != "pve" {
					t.Fatalf("guest identity/state mixed: %+v", ref)
				}
				delete(want, key)
			}
		})
	}
}

func TestPVEAttachDetachPreserveForeignDisk(t *testing.T) {
	req, err := domain.NewRequest("safe", domain.Placement{Node: "pve", Storage: "pool"}, 7777, domain.GiB, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := domain.ParseHandle(req.Handle())
	uuid, _ := domain.ParseNodeID("153a7c9e-588b-483f-884a-4eba01bc160a")
	vm := domain.VirtualMachine{Node: "pve", VMID: 100, ID: uuid}
	attached := false
	writes := 0
	diskSerial := id.Serial()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var result any
		switch {
		case r.URL.Path == "/api2/json/access/permissions":
			result = map[string]any{"/vms/100": map[string]int{"VM.Config.Disk": 0}, "/nodes/pve": map[string]int{"Sys.Audit": 0}}
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			result = []any{}
		case strings.HasSuffix(r.URL.Path, "/pending"):
			result = []any{}
		case strings.HasSuffix(r.URL.Path, "/config") && r.Method == "GET":
			cfg := map[string]any{"digest": "abc", "smbios1": "uuid=" + uuid.String(), "scsihw": "virtio-scsi-pci", "scsi0": "pool:vm-100-disk-0,size=32G"}
			if attached {
				cfg["scsi1"] = id.Reference() + ",serial=" + diskSerial + ",backup=0"
			}
			result = cfg
		case strings.HasSuffix(r.URL.Path, "/config") && r.Method == "POST":
			_ = r.ParseForm()
			writes++
			if r.Form.Get("digest") != "abc" || r.Form.Has("force") || r.Form.Has("scsi0") {
				t.Error("unsafe VM update")
			}
			if !attached {
				if r.Form.Get("scsi1") != id.Reference()+",serial="+id.Serial()+",backup=0" {
					t.Error("wrong attach disk")
				}
				attached = true
			} else {
				if r.Form.Get("delete") != "scsi1" {
					t.Error("detach must remove only the managed SCSI slot")
				}
				attached = false
			}
			result = "UPID:pve:00000001:00000001:00000001:qmconfig:100:test:"
		case strings.HasSuffix(r.URL.Path, "/status"):
			result = map[string]string{"upid": "UPID:pve:00000001:00000001:00000001:qmconfig:100:test:", "type": "qmconfig", "id": "100", "status": "stopped", "exitstatus": "OK"}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": result})
	}))
	defer srv.Close()
	g, _ := NewAttachmentGateway(&Client{httpClient: srv.Client(), config: Config{Endpoint: srv.URL}})
	if err := g.Attach(context.Background(), id, vm); err != nil {
		t.Fatal(err)
	}
	ref := domain.DiskReference{Source: vm.ReferenceSource(), Slot: "scsi1", Serial: id.Serial(), State: domain.ReferenceAttached}
	diskSerial = "foreign"
	if err := g.Detach(context.Background(), id, ref); !errors.Is(err, domain.ErrAttachmentConflict) || writes != 1 || !attached {
		t.Fatalf("stale reference detached a changed disk: %v", err)
	}
	diskSerial = id.Serial()
	if err := g.Detach(context.Background(), id, ref); err != nil {
		t.Fatal(err)
	}
	if writes != 2 || attached {
		t.Fatal("lifecycle did not complete")
	}
	ref.Slot = "unused0"
	if err := g.Detach(context.Background(), id, ref); !errors.Is(err, domain.ErrAttachmentConflict) {
		t.Fatal(err)
	}
	ref.Slot = "scsi0"
	if err := g.Detach(context.Background(), id, ref); !errors.Is(err, domain.ErrAttachmentConflict) {
		t.Fatal(err)
	}
	if writes != 2 {
		t.Fatal("unsafe slot was written")
	}
}

func TestReferenceParsing(t *testing.T) {
	req, err := domain.NewRequest("safe", domain.Placement{Node: "pve", Storage: "pool"}, 7777, domain.GiB, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := domain.ParseHandle(req.Handle())
	for _, state := range []domain.ReferenceState{domain.ReferenceAttached, domain.ReferencePending, domain.ReferenceSnapshot} {
		cfg := vmConfig{Drives: map[string]string{"scsi1": id.Reference() + ",serial=" + id.Serial()}}
		refs, err := referencesIn(cfg, id, domain.ReferenceSource{}, state)
		if err != nil || len(refs) != 1 || refs[0].State != state || refs[0].Serial != id.Serial() {
			t.Fatal(refs, err)
		}
	}
	refs, err := referencesIn(vmConfig{Drives: map[string]string{"unused0": id.Reference()}}, id, domain.ReferenceSource{}, domain.ReferenceAttached)
	if err != nil || len(refs) != 1 || refs[0].State != domain.ReferenceUnused {
		t.Fatal(refs, err)
	}
}

func TestMalformedVMConfigurationStopsAttachment(t *testing.T) {
	for _, field := range []string{"lock", "hotplug", "scsi0"} {
		t.Run(field, func(t *testing.T) {
			writes := 0
			uuid, _ := domain.ParseNodeID("153a7c9e-588b-483f-884a-4eba01bc160a")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var data any
				switch {
				case r.Method == "POST":
					writes++
					data = "UPID:pve:1:1:1:qmconfig:100:test:"
				case strings.HasSuffix(r.URL.Path, "/permissions"):
					data = map[string]any{"/vms/100": map[string]int{"VM.Config.Disk": 0}, "/nodes/pve": map[string]int{"Sys.Audit": 0}}
				case strings.HasSuffix(r.URL.Path, "/tasks"), strings.HasSuffix(r.URL.Path, "/pending"):
					data = []any{}
				case strings.HasSuffix(r.URL.Path, "/config"):
					data = map[string]any{"digest": "abc", "smbios1": "uuid=" + uuid.String(), "scsihw": "virtio-scsi-pci", field: 123}
				case strings.HasSuffix(r.URL.Path, "/status"):
					data = map[string]string{"upid": "UPID:pve:1:1:1:qmconfig:100:test:", "type": "qmconfig", "id": "100", "status": "stopped", "exitstatus": "OK"}
				default:
					t.Errorf("unexpected request %s", r.URL)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))
			defer srv.Close()
			g, _ := NewAttachmentGateway(&Client{httpClient: srv.Client(), config: Config{Endpoint: srv.URL}})
			req, err := domain.NewRequest("safe", domain.Placement{Node: "pve", Storage: "pool"}, 7777, domain.GiB, 0)
			if err != nil {
				t.Fatal(err)
			}
			id, _ := domain.ParseHandle(req.Handle())
			err = g.Attach(context.Background(), id, domain.VirtualMachine{Node: "pve", VMID: 100, ID: uuid})
			if !errors.Is(err, domain.ErrInvalidInventory) || writes != 0 {
				t.Fatalf("malformed %s allowed a write: writes=%d err=%v", field, writes, err)
			}
		})
	}
}

func TestReferenceScanDistinguishesSources(t *testing.T) {
	req, err := domain.NewRequest("safe", domain.Placement{Node: "pve", Storage: "pool"}, 7777, domain.GiB, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := domain.ParseHandle(req.Handle())
	for _, tc := range []struct {
		name, location, kind, uuid string
		template                   int
		want                       int
		source                     domain.SourceKind
		invalid                    bool
	}{
		{"unrelated template", "", "qemu", "", 1, 0, domain.SourceTemplate, false},
		{"template disk", "current", "qemu", "", 1, 1, domain.SourceTemplate, false},
		{"template with UUID", "current", "qemu", "153a7c9e-588b-483f-884a-4eba01bc160a", 1, 1, domain.SourceTemplate, false},
		{"template pending disk", "pending", "qemu", "", 1, 1, domain.SourceTemplate, false},
		{"template snapshot disk", "snapshot", "qemu", "", 1, 1, domain.SourceTemplate, false},
		{"container disk", "current", "lxc", "", 0, 1, domain.SourceContainer, false},
		{"container pending disk", "pending", "lxc", "", 0, 1, domain.SourceContainer, false},
		{"container snapshot disk", "snapshot", "lxc", "", 0, 1, domain.SourceContainer, false},
		{"referencing VM missing UUID", "current", "qemu", "", 0, 0, domain.SourceVM, true},
		{"referencing VM with UUID", "current", "qemu", "153a7c9e-588b-483f-884a-4eba01bc160a", 0, 1, domain.SourceVM, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var data any = []any{}
				switch {
				case strings.HasSuffix(r.URL.Path, "/permissions"):
					data = map[string]any{"/vms": map[string]int{"VM.Audit": 1}, "/vms/9000": map[string]int{"VM.Audit": 1}, "/nodes/pve": map[string]int{"Sys.Audit": 0}}
				case strings.HasSuffix(r.URL.Path, "/"+tc.kind):
					data = []any{map[string]int{"vmid": 9000, "template": tc.template}}
				case strings.HasSuffix(r.URL.Path, "/config"):
					cfg := map[string]any{"digest": "abc", "template": tc.template}
					if tc.uuid != "" {
						cfg["smbios1"] = "uuid=" + tc.uuid
					}
					if tc.location == "current" || strings.Contains(r.URL.Path, "/snapshot/") {
						cfg["scsi1"] = id.Reference() + ",serial=" + id.Serial()
					}
					data = cfg
				case strings.HasSuffix(r.URL.Path, "/pending") && tc.location == "pending":
					data = []any{map[string]any{"key": "scsi1", "pending": id.Reference() + ",serial=" + id.Serial()}}
				case strings.HasSuffix(r.URL.Path, "/snapshot") && tc.location == "snapshot":
					data = []any{map[string]string{"name": "saved"}}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))
			defer srv.Close()
			g, _ := NewAttachmentGateway(&Client{httpClient: srv.Client(), config: Config{Endpoint: srv.URL}})
			refs, err := g.References(context.Background(), id)
			if (errors.Is(err, domain.ErrInvalidInventory) || errors.Is(err, errMissingVMUUID)) != tc.invalid || (!tc.invalid && (err != nil || len(refs) != tc.want)) {
				t.Fatalf("references=%v error=%v", refs, err)
			}
			for _, ref := range refs {
				if ref.Source.Kind != tc.source || ref.Source.Node != "pve" || ref.Source.VMID != 9000 || ref.Source.NodeID.String() != tc.uuid {
					t.Fatalf("wrong reference source: %+v", ref.Source)
				}
				_, vmErr := ref.Source.VirtualMachine()
				if (vmErr == nil) != (tc.source == domain.SourceVM) {
					t.Fatalf("reference converted to wrong target type: %+v %v", ref.Source, vmErr)
				}
			}
			if len(refs) != 0 && !errors.Is(refs.CheckDelete(), domain.ErrVolumeInUse) {
				t.Fatal("referenced disk is deletable")
			}

		})
	}
}

func TestConfigRejectsMalformedFieldsAndUUID(t *testing.T) {
	for _, raw := range []string{`null`, `{"lock":null}`, `{"template":null}`, `{"template":2}`, `{"scsi1":null}`, `{"scsi1":""}`} {
		var cfg vmConfig
		if err := json.Unmarshal([]byte(raw), &cfg); !errors.Is(err, domain.ErrInvalidInventory) {
			t.Fatalf("accepted %s: %v", raw, err)
		}
	}
	for _, raw := range []string{"uuid=invalid", "uuid=,uuid=153a7c9e-588b-483f-884a-4eba01bc160a", "uuid=153a7c9e-588b-483f-884a-4eba01bc160a,uuid=153a7c9e-588b-483f-884a-4eba01bc160a"} {
		if _, err := (vmConfig{SMBIOS: raw}).nodeID(); !errors.Is(err, domain.ErrInvalidInventory) {
			t.Fatal("invalid backend UUID must not be a client input error", err)
		}
	}
}
