package csidriver

import "github.com/container-storage-interface/spec/lib/go/csi"

// Shared policy for creation and validation. This does not inspect a guest
// filesystem or imply that Node/attachment support is already implemented.
func supportedMountCapability(capability *csi.VolumeCapability) bool {
	mount := capability.GetMount()
	return mount != nil && capability.GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER &&
		(mount.FsType == "" || mount.FsType == "ext4") && len(mount.MountFlags) == 0 && mount.VolumeMountGroup == ""
}
