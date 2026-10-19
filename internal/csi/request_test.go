package csidriver

import (
	"context"
	"math"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

func TestCreateRequestRejectsUnsafeInputBeforeBackendIO(t *testing.T) {
	// Embedded nil ports intentionally panic if validation reaches backend IO.
	service, err := volume.NewService(struct{ volume.StorageGateway }{}, domain.Scope{Node: "pve", OwnerVMID: 7777}, struct{ volume.AttachmentGateway }{})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(Config{DriverName: "example.csi"}, service)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*csi.CreateVolumeRequest)
		code   codes.Code
	}{
		{"empty name", func(r *csi.CreateVolumeRequest) { r.Name = "" }, codes.InvalidArgument},
		{"invalid name", func(r *csi.CreateVolumeRequest) { r.Name = "bad\x00name" }, codes.InvalidArgument},
		{"missing storage", func(r *csi.CreateVolumeRequest) { r.Parameters = nil }, codes.InvalidArgument},
		{"negative capacity", func(r *csi.CreateVolumeRequest) { r.CapacityRange = &csi.CapacityRange{RequiredBytes: -1} }, codes.InvalidArgument},
		{"reversed bounds", func(r *csi.CreateVolumeRequest) {
			r.CapacityRange = &csi.CapacityRange{RequiredBytes: 2 * domain.GiB, LimitBytes: domain.GiB}
		}, codes.InvalidArgument},
		{"rounding exceeds limit", func(r *csi.CreateVolumeRequest) {
			r.CapacityRange = &csi.CapacityRange{RequiredBytes: domain.GiB + 1, LimitBytes: domain.GiB + 1}
		}, codes.OutOfRange},
		{"rounding overflow", func(r *csi.CreateVolumeRequest) { r.CapacityRange = &csi.CapacityRange{RequiredBytes: math.MaxInt64} }, codes.OutOfRange},
		{"unsupported access", func(r *csi.CreateVolumeRequest) {
			r.VolumeCapabilities[0].AccessMode.Mode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
		}, codes.InvalidArgument},
		{"mount flags", func(r *csi.CreateVolumeRequest) { r.VolumeCapabilities[0].GetMount().MountFlags = []string{"discard"} }, codes.InvalidArgument},
		{"unknown topology key", func(r *csi.CreateVolumeRequest) {
			r.AccessibilityRequirements = &csi.TopologyRequirement{Requisite: []*csi.Topology{{Segments: map[string]string{"wrong/key": "pve"}}}}
		}, codes.InvalidArgument},
		{"wrong PVE node", func(r *csi.CreateVolumeRequest) {
			r.AccessibilityRequirements = &csi.TopologyRequirement{Requisite: []*csi.Topology{{Segments: map[string]string{"example.csi/pve-node": "other"}}}}
		}, codes.ResourceExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &csi.CreateVolumeRequest{Name: "pvc-one", Parameters: map[string]string{"storage": "pool"}, VolumeCapabilities: []*csi.VolumeCapability{{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}}, AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}}}}
			tc.change(req)
			response, err := controller.CreateVolume(context.Background(), req)
			if response != nil || status.Code(err) != tc.code {
				t.Fatalf("response=%v error=%v", response, err)
			}
		})
	}
}

func TestCSIAndApplicationUseTheSameVolumeIdentity(t *testing.T) {
	service, err := volume.NewService(struct{ volume.StorageGateway }{}, domain.Scope{Node: "pve", OwnerVMID: 7777}, struct{ volume.AttachmentGateway }{})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(Config{DriverName: "example.csi"}, service)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pvc-one", " one\t\n\r", "csi-v1-original-name"} {
		req := &csi.CreateVolumeRequest{Name: name, Parameters: map[string]string{"storage": "pool"}, VolumeCapabilities: []*csi.VolumeCapability{{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}}, AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}}}}
		fromCSI, err := controller.volumeRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		direct, err := domain.NewRequest(name, domain.Placement{Node: "pve", Storage: "pool"}, 7777, domain.GiB, 0)
		if err != nil || fromCSI.Handle() != direct.Handle() {
			t.Fatalf("entry points disagree for %q: %q vs %q: %v", name, fromCSI.Handle(), direct.Handle(), err)
		}
	}
}
