package bootstrap

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"pve-csi.local/csi-plugin/internal/testutil"
	"pve-csi.local/csi-plugin/internal/volume/domain"
	csidriver "pve-csi.local/csi-plugin/internal/volume/interfaces/csi"
)

func TestControllerCompositionCreatesThenValidatesAfterRestart(t *testing.T) {
	backend := testutil.VolumeRequest()
	backend.Name = domain.ProvisioningName("abc")
	api, client := testutil.NewVolumeAPI(t, backend)
	cfg := csidriver.Config{Node: backend.Node, Storage: backend.Storage, OwnerVMID: backend.OwnerVMID}
	controller, err := NewController(cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := []*csi.VolumeCapability{{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}}
	created, err := controller.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "abc", CapacityRange: &csi.CapacityRange{RequiredBytes: backend.CapacityBytes}, VolumeCapabilities: capabilities,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.GetVolume().GetVolumeId() != backend.Handle() {
		t.Fatal("creation handle changed")
	}
	for i := 0; i < 2; i++ {
		response, err := controller.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
			VolumeId: created.Volume.VolumeId, VolumeCapabilities: capabilities,
		})
		if err != nil || response.GetConfirmed() == nil {
			t.Fatalf("validation = %v, %v", response, err)
		}
		// A fresh composition must recover the volume through its handle,
		// without any original request-name cache or remembered capacity.
		controller, err = NewController(cfg, client)
		if err != nil {
			t.Fatal(err)
		}
	}
	api.AssertCounts(t, 4, 1)
}
