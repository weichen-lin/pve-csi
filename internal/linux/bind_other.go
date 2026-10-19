//go:build !linux

package linux

import "golang.org/x/sys/unix"

func openDetachedMount(string) (mountTree, error) { return nil, unix.ENOSYS }
