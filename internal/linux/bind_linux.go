package linux

import "golang.org/x/sys/unix"

type detachedMount struct{ fd int }

func openDetachedMount(stage string) (mountTree, error) {
	fd, err := unix.OpenTree(unix.AT_FDCWD, stage, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return nil, err
	}
	return detachedMount{fd: fd}, nil
}

func (m detachedMount) ReadOnly() error {
	return unix.MountSetattr(m.fd, "", unix.AT_EMPTY_PATH, &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY})
}

func (m detachedMount) Publish(target string) error {
	return unix.MoveMount(m.fd, "", unix.AT_FDCWD, target, unix.MOVE_MOUNT_F_EMPTY_PATH)
}

func (m detachedMount) Close() error { return unix.Close(m.fd) }
