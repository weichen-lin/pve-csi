package csidriver

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
)

type readyWriter chan struct{}

func socketTestPath(t *testing.T) string {
	t.Helper()
	// macOS Unix socket paths must fit in 104 bytes.
	dir, err := os.MkdirTemp("/tmp", "csi-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "csi.sock")
}

func (w readyWriter) Write(p []byte) (int, error) {
	close(w)
	return len(p), nil
}

func startSocketServer(t *testing.T, path string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(readyWriter)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, "unix://"+path, grpc.NewServer(), ready) }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("server did not start: %v", err)
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("server startup timed out")
	}
	return func() {
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestServeRecoversStaleSocket(t *testing.T) {
	path := socketTestPath(t)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false) // Simulate the socket left behind by SIGKILL.
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	stop := startSocketServer(t, path)
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		stop()
		t.Fatal(err)
	}
	conn.Close()
	stop()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("graceful shutdown left socket: %v", err)
	}
}

func TestServePreservesExistingEndpoints(t *testing.T) {
	for _, kind := range []string{"live socket", "file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			path := socketTestPath(t)
			switch kind {
			case "live socket":
				l, err := net.Listen("unix", path)
				if err != nil {
					t.Fatal(err)
				}
				defer l.Close()
			case "file":
				if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("missing-target", path); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := Serve(context.Background(), "unix://"+path, grpc.NewServer(), io.Discard); err == nil {
				t.Fatal("accepted occupied endpoint")
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("existing endpoint changed: %v", err)
			}
		})
	}
}

func TestServeExcludesSecondInstance(t *testing.T) {
	path := socketTestPath(t)
	stop := startSocketServer(t, path)
	if err := Serve(context.Background(), "unix://"+path, grpc.NewServer(), io.Discard); err == nil {
		t.Fatal("second instance took over endpoint")
	}
	stop()
	startSocketServer(t, path)()
}
