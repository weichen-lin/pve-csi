package linux

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/sys/unix"
	"pve-csi.local/csi-plugin/internal/volume"
)

type startupTree struct {
	readonlyErr error
	closes      int
	published   bool
}

func (t *startupTree) ReadOnly() error      { return t.readonlyErr }
func (t *startupTree) Publish(string) error { t.published = true; return nil }
func (t *startupTree) Close() error         { t.closes++; return nil }

func TestNodeStartupChecksBeforeAnyMountIsPublished(t *testing.T) {
	for _, tc := range []struct {
		name string
		want volume.FailureKind
	}{
		{"missing tool", volume.FailurePrecondition},
		{"mount permission", volume.FailurePermissionDenied},
		{"old kernel", volume.FailurePrecondition},
		{"ready", volume.FailureInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := &startupTree{}
			opens := 0
			g := NewMountGateway("example.csi")
			g.directory = func(_ string, create bool) error {
				if create {
					t.Error("startup created directory")
				}
				return nil
			}
			g.openTree = func(string) (mountTree, error) {
				opens++
				if tc.name == "mount permission" {
					return nil, unix.EPERM
				}
				return tree, nil
			}
			if tc.name == "old kernel" {
				tree.readonlyErr = unix.ENOSYS
			}
			err := g.checkStartup(context.Background(), func(name string) (string, error) {
				if tc.name == "missing tool" && name == "mkfs.ext4" {
					return "", errors.New("not installed")
				}
				return "/bin/" + name, nil
			})
			if tc.name == "ready" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var backend *volume.BackendError
				if !errors.As(err, &backend) || backend.Kind() != tc.want {
					t.Fatalf("wrong startup error: %v", err)
				}
			}
			if tree.published {
				t.Fatal("startup published a mount")
			}
			if tc.name == "missing tool" && opens != 0 {
				t.Fatal("mount checked before missing tools")
			}
			if (tc.name == "ready" || tc.name == "old kernel") && tree.closes != 1 {
				t.Fatal("detached startup mount leaked")
			}
		})
	}
}
