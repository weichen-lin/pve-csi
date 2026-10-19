//go:build linux && integration

package linux

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"pve-csi.local/csi-plugin/internal/volume"
)

func TestNodeStartupWithMountPrivileges(t *testing.T) {
	before, err := mounts()
	if err != nil {
		t.Fatal(err)
	}
	if err := NewMountGateway("example.csi").CheckStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := mounts()
	if err != nil || len(after) != len(before) {
		t.Fatalf("startup changed mounted filesystems: %v", err)
	}
}

func TestNodeStartupWithoutMountPrivileges(t *testing.T) {
	err := NewMountGateway("example.csi").CheckStartup(context.Background())
	var backend *volume.BackendError
	if !errors.As(err, &backend) || backend.Kind() != volume.FailurePermissionDenied {
		t.Fatalf("missing startup permission check: %v", err)
	}
}

// Run explicitly with -tags=integration in an isolated Linux container with
// CAP_SYS_ADMIN. Only tmpfs mounts in a private namespace are created; no disks.
func TestAtomicBindSurvivesSIGKILL(t *testing.T) {
	if phase := os.Getenv("CSI_BIND_CHILD_PHASE"); phase != "" {
		tree, err := openDetachedMount(os.Getenv("CSI_BIND_STAGE"))
		if err != nil {
			t.Fatal(err)
		}
		defer tree.Close()
		if phase != "clone" {
			if err := tree.ReadOnly(); err != nil {
				t.Fatal(err)
			}
		}
		if phase == "published" {
			if err := tree.Publish(os.Getenv("CSI_BIND_TARGET")); err != nil {
				t.Fatal(err)
			}
		}
		fmt.Println("ready-to-kill")
		for {
			_ = unix.Pause()
		}
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		t.Fatalf("isolated mount namespace required: %v", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := unix.Mount("tmpfs", root, "tmpfs", 0, "size=4m"); err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount(root, unix.MNT_DETACH)
	// Like kubelet's bidirectionally propagated volume, the parent is shared.
	if err := unix.Mount("", root, "", unix.MS_SHARED, ""); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, "stage")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("tmpfs", stage, "tmpfs", 0, "size=1m"); err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount(stage, 0)
	if err := os.WriteFile(filepath.Join(stage, "data"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"clone", "readonly", "published"} {
		target := filepath.Join(root, phase)
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestAtomicBindSurvivesSIGKILL$")
		cmd.Env = append(os.Environ(), "CSI_BIND_CHILD_PHASE="+phase, "CSI_BIND_STAGE="+stage, "CSI_BIND_TARGET="+target)
		cmd.Stderr = os.Stderr
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		ready := make(chan bool, 1)
		go func() {
			scanner := bufio.NewScanner(stdout)
			ready <- scanner.Scan() && scanner.Text() == "ready-to-kill"
		}()
		select {
		case ok := <-ready:
			if !ok {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatal("child failed before checkpoint")
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("child checkpoint timed out")
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		list, err := mounts()
		if err != nil {
			t.Fatal(err)
		}
		m, found, err := mountAt(list, target)
		if err != nil || found != (phase == "published") {
			t.Fatalf("%s: unexpected mount after kill: %v %v", phase, found, err)
		}
		if found && !readOnly(m) {
			t.Fatal("SIGKILL left writable target")
		}
		if !found {
			if err := bindMount(context.Background(), openDetachedMount, stage, target, true); err != nil {
				t.Fatal(err)
			}
		}
		data, err := os.ReadFile(filepath.Join(target, "data"))
		if err != nil || string(data) != "keep" {
			t.Fatalf("published data changed: %q %v", data, err)
		}
		if err := os.WriteFile(filepath.Join(target, "forbidden"), []byte("bad"), 0600); !errors.Is(err, unix.EROFS) {
			t.Fatalf("readonly bind allowed write: %v", err)
		}
		if err := os.WriteFile(filepath.Join(stage, "still-writable"), nil, 0600); err != nil {
			t.Fatalf("changed stage access mode: %v", err)
		}
		if err := unix.Unmount(target, 0); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(root, "writable")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := bindMount(context.Background(), openDetachedMount, stage, target, false); err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount(target, 0)
	if err := os.WriteFile(filepath.Join(target, "written-through-publish"), []byte("persist"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(stage, "written-through-publish"))
	if err != nil || string(data) != "persist" {
		t.Fatalf("writable bind did not share data: %q %v", data, err)
	}
}
