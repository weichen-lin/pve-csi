package linux

import (
	"context"
	"os/exec"

	"pve-csi.local/csi-plugin/internal/volume"
)

// CheckStartup makes missing tools and mount privileges startup failures,
// before a request can format a disk. Only an unpublished clone is inspected;
// no disk, existing mount, or Pod target is modified.
func (g *MountGateway) CheckStartup(ctx context.Context) error {
	return g.checkStartup(ctx, exec.LookPath)
}

func (g *MountGateway) checkStartup(ctx context.Context, lookPath func(string) (string, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, name := range []string{"lsblk", "blkid", "wipefs", "mkfs.ext4", "mount", "umount"} {
		if _, err := lookPath(name); err != nil {
			return volume.AtStep("check_node_tools", name, volume.NewBackendError(volume.FailurePrecondition, "required executable "+name+" is missing or not executable; check the node image", err))
		}
	}
	if err := g.directory("/var/lib/kubelet", false); err != nil {
		return volume.AtStep("check_node_mount_support", "/var/lib/kubelet", volume.NewBackendError(volume.FailurePrecondition, "cannot inspect kubelet directory; check node host mounts and permissions", err))
	}
	tree, err := g.openTree("/var/lib/kubelet")
	if err != nil {
		return mountAPIError("check_node_mount_support", err)
	}
	defer tree.Close()
	if err := tree.ReadOnly(); err != nil {
		return mountAPIError("check_node_mount_readonly", err)
	}
	return ctx.Err()
}
