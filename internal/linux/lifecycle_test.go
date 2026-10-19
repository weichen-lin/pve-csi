package linux

import (
	"context"
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

// Kernel state survives a new gateway instance; pending detached trees do not.
type mountHost struct {
	list                          map[string]mount
	disk                          device
	formatted                     bool
	formats, publications, closes int
	crashAt                       string
	failAt                        string
	signatures                    string
}

func (h *mountHost) checkpoint(step string) error {
	if h.crashAt == step {
		h.crashAt = ""
		panic("simulated process death")
	}
	if h.failAt == step {
		return context.DeadlineExceeded
	}
	return nil
}

func (h *mountHost) gateway() *MountGateway {
	return &MountGateway{driver: "example.csi", hostOperations: hostOperations{
		device:     func(context.Context, domain.Identity) (device, error) { return h.disk, nil },
		filesystem: func(context.Context, device, domain.Identity) (bool, error) { return h.formatted, nil },
		mounts: func() ([]mount, error) {
			var list []mount
			for _, m := range h.list {
				list = append(list, m)
			}
			return list, nil
		},
		directory: func(string, bool) error { return nil },
		readDir:   func(string) ([]os.DirEntry, error) { return nil, nil },
		removeEmpty: func(path string) error {
			if _, found := h.list[path]; found {
				return volume.ErrMountInUse
			}
			return nil
		},
		openTree: func(stage string) (mountTree, error) {
			m, found := h.list[stage]
			if !found {
				return nil, os.ErrNotExist
			}
			return &fakeMountTree{host: h, mount: m}, nil
		},
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			path := args[len(args)-1]
			switch name {
			case "wipefs":
				return []byte(h.signatures), nil
			case "mkfs.ext4":
				h.formats++
				h.formatted = true
			case "mount":
				h.list[path] = mount{Path: path, Root: "/", Number: h.disk.Number, FSType: "ext4", Options: "rw"}
			case "umount":
				delete(h.list, path)
			default:
				return nil, errors.New("unexpected command")
			}
			return nil, h.checkpoint(name)
		},
	}}
}

type fakeMountTree struct {
	host  *mountHost
	mount mount
}

func (m *fakeMountTree) ReadOnly() error {
	m.mount.Options = "ro"
	return m.host.checkpoint("readonly")
}
func (m *fakeMountTree) Publish(target string) error {
	if err := m.host.checkpoint("before_publish"); err != nil {
		return err
	}
	m.mount.Path = target
	m.host.list[target] = m.mount
	m.host.publications++
	return m.host.checkpoint("after_publish")
}
func (m *fakeMountTree) Close() error { m.host.closes++; return nil }

func mountFixture(t *testing.T) (*mountHost, domain.Identity, string, string) {
	t.Helper()
	req, err := domain.NewRequest("one", domain.Placement{Node: "pve", Storage: "pool"}, 7777, domain.GiB, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, err := domain.ParseHandle(req.Handle())
	if err != nil {
		t.Fatal(err)
	}
	h := &mountHost{list: map[string]mount{}, disk: device{Name: "sdb", Number: "8:16", Serial: id.Serial(), Type: "disk"}, signatures: `{"signatures":[]}`}
	return h, id, h.gateway().stagingPath(id), "/var/lib/kubelet/pods/153a7c9e-588b-483f-884a-4eba01bc160a/volumes/kubernetes.io~csi/pv/mount"
}

func crash(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Error("crash point was not reached")
		}
	}()
	fn()
}

func TestMountLifecycleRecoversAfterProcessDeath(t *testing.T) {
	for _, point := range []string{"mkfs.ext4", "mount", "readonly", "after_publish", "unpublish", "unstage"} {
		t.Run(point, func(t *testing.T) {
			h, id, stage, target := mountFixture(t)
			ctx := context.Background()
			g := h.gateway()
			if point == "mkfs.ext4" || point == "mount" {
				h.crashAt = point
				crash(t, func() { _ = g.Stage(ctx, id, stage) })
			}
			g = h.gateway()
			if err := g.Stage(ctx, id, stage); err != nil {
				t.Fatal(err)
			}
			if point == "readonly" || point == "after_publish" {
				h.crashAt = point
				crash(t, func() { _ = g.Publish(ctx, id, stage, target, true) })
				if m, found := h.list[target]; found && !readOnly(m) {
					t.Fatal("crash exposed writable target")
				}
			}
			g = h.gateway()
			for range 2 {
				if err := g.Publish(ctx, id, stage, target, true); err != nil {
					t.Fatal(err)
				}
			}
			if h.formats != 1 || h.publications != 1 {
				t.Fatalf("repeated destructive effect: formats=%d binds=%d", h.formats, h.publications)
			}
			if err := g.Unstage(ctx, id, stage); !errors.Is(err, volume.ErrMountInUse) {
				t.Fatalf("removed stage while published: %v", err)
			}
			if point == "unpublish" {
				h.crashAt = "umount"
				crash(t, func() { _ = g.Unpublish(ctx, id, target) })
			}
			g = h.gateway()
			for range 2 {
				if err := g.Unpublish(ctx, id, target); err != nil {
					t.Fatal(err)
				}
			}
			if point == "unstage" {
				h.crashAt = "umount"
				crash(t, func() { _ = g.Unstage(ctx, id, stage) })
			}
			g = h.gateway()
			for range 2 {
				if err := g.Unstage(ctx, id, stage); err != nil {
					t.Fatal(err)
				}
			}
			if len(h.list) != 0 {
				t.Fatal("mounts leaked after cleanup")
			}
		})
	}
}

func TestReadonlyPreparationFailureLeavesNoTarget(t *testing.T) {
	for _, point := range []string{"readonly", "before_publish"} {
		t.Run(point, func(t *testing.T) {
			h, id, stage, target := mountFixture(t)
			g := h.gateway()
			if err := g.Stage(context.Background(), id, stage); err != nil {
				t.Fatal(err)
			}
			h.failAt = point
			if err := g.Publish(context.Background(), id, stage, target, true); err == nil {
				t.Fatal("failure hidden")
			}
			if _, found := h.list[target]; found || h.closes != 1 {
				t.Fatal("unpublished tree leaked")
			}
			h.failAt = ""
			if err := h.gateway().Publish(context.Background(), id, stage, target, true); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWritablePublishRetryAndCleanup(t *testing.T) {
	h, id, stage, target := mountFixture(t)
	ctx := context.Background()
	g := h.gateway()
	if err := g.Stage(ctx, id, stage); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := h.gateway().Publish(ctx, id, stage, target, false); err != nil {
			t.Fatal(err)
		}
	}
	if readOnly(h.list[target]) || h.publications != 1 {
		t.Fatal("writable mount changed or duplicated on retry")
	}
	if err := g.Unpublish(ctx, id, target); err != nil {
		t.Fatal(err)
	}
	if err := g.Unstage(ctx, id, stage); err != nil || len(h.list) != 0 {
		t.Fatalf("cleanup: %v", err)
	}
}

func TestMountLifecyclePreservesConflictingTargets(t *testing.T) {
	for _, conflict := range []string{"wrong device", "writable mount"} {
		t.Run(conflict, func(t *testing.T) {
			h, id, stage, target := mountFixture(t)
			g := h.gateway()
			if err := g.Stage(context.Background(), id, stage); err != nil {
				t.Fatal(err)
			}
			m := mount{Path: target, Root: "/", Number: h.disk.Number, FSType: "ext4", Options: "rw"}
			want := volume.ErrMountConflict
			if conflict == "wrong device" {
				m.Number = "8:0"
				want = volume.ErrUnsafeMount
			}
			h.list[target] = m
			if err := g.Publish(context.Background(), id, stage, target, true); !errors.Is(err, want) {
				t.Fatal(err)
			}
			if h.list[target] != m || h.publications != 0 {
				t.Fatal("existing mount changed")
			}
			if conflict == "wrong device" {
				if err := g.Unpublish(context.Background(), id, target); !errors.Is(err, volume.ErrUnsafeMount) {
					t.Fatal(err)
				}
				if h.list[target] != m {
					t.Fatal("unmounted wrong disk")
				}
			}
		})
	}
}

func TestStageNeverFormatsDiskWithExistingSignature(t *testing.T) {
	h, id, stage, _ := mountFixture(t)
	h.signatures = `{"signatures":[{"type":"LVM2_member"}]}`
	if err := h.gateway().Stage(context.Background(), id, stage); !errors.Is(err, volume.ErrUnsafeMount) {
		t.Fatal(err)
	}
	if h.formats != 0 || len(h.list) != 0 {
		t.Fatal("foreign disk modified")
	}
}

func TestAtomicMountUnavailableFailsWithoutFallback(t *testing.T) {
	h, id, stage, target := mountFixture(t)
	g := h.gateway()
	if err := g.Stage(context.Background(), id, stage); err != nil {
		t.Fatal(err)
	}
	g.openTree = func(string) (mountTree, error) { return nil, unix.ENOSYS }
	err := g.Publish(context.Background(), id, stage, target, true)
	var backend *volume.BackendError
	if !errors.As(err, &backend) || backend.Kind() != volume.FailurePrecondition || h.publications != 0 {
		t.Fatal(err)
	}
}

func TestRejectedMountRequestDoesNotCreateDirectories(t *testing.T) {
	for _, operation := range []string{"stage missing device", "stage unsafe filesystem", "publish missing stage"} {
		t.Run(operation, func(t *testing.T) {
			h, id, stage, target := mountFixture(t)
			g := h.gateway()
			created := 0
			g.directory = func(_ string, create bool) error {
				if create {
					created++
				}
				return nil
			}
			switch operation {
			case "stage missing device":
				g.device = func(context.Context, domain.Identity) (device, error) { return device{}, volume.ErrDeviceNotFound }
			case "stage unsafe filesystem":
				g.filesystem = func(context.Context, device, domain.Identity) (bool, error) { return false, volume.ErrUnsafeMount }
			case "publish missing stage":
				h.formatted = true
			}
			var err error
			if operation == "publish missing stage" {
				err = g.Publish(context.Background(), id, stage, target, true)
			} else {
				err = g.Stage(context.Background(), id, stage)
			}
			if err == nil || created != 0 || h.formats != 0 || h.publications != 0 {
				t.Fatalf("rejected request had effects: err=%v created=%d", err, created)
			}
		})
	}
}
