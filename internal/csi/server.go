package csidriver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// SocketPath deliberately supports local Unix sockets only. The CSI endpoint
// has no network authentication and must not be exposed as a TCP service.
func SocketPath(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "unix" || u.Host != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" ||
		!filepath.IsAbs(u.Path) || u.Path == "/" || filepath.Clean(u.Path) != u.Path || strings.ContainsRune(u.Path, 0) || endpoint != "unix://"+u.Path {
		return "", errors.New("CSI endpoint must be unix:///absolute/path without query, fragment or escaped characters")
	}
	return u.Path, nil
}

// Serve serializes socket ownership across restarts and reclaims only a socket
// that refuses connections. Shutdown is bounded even if a PVE request stalls.
func Serve(ctx context.Context, endpoint string, server *grpc.Server, out io.Writer) error {
	path, err := SocketPath(endpoint)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Keep the lock file: unlinking it would allow another process to lock a
	// different inode. The kernel releases this lock even after SIGKILL.
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open CSI socket lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("lock CSI socket (another instance may be running): %w", err)
	}
	if err := removeStaleSocket(path); err != nil {
		return fmt.Errorf("prepare CSI socket: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen on CSI socket: %w", err)
	}
	defer listener.Close()
	defer server.Stop()
	if err := os.Chmod(path, 0600); err != nil {
		return fmt.Errorf("protect CSI socket: %w", err)
	}
	if _, err := fmt.Fprintf(out, "CSI listening endpoint=%s\n", endpoint); err != nil {
		return err
	}
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			timer := time.AfterFunc(10*time.Second, server.Stop)
			server.GracefulStop()
			timer.Stop()
		case <-finished:
		}
	}()
	err = server.Serve(listener)
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("serve CSI: %w", err)
	}
	return nil
}

func removeStaleSocket(path string) error {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if before.Mode()&os.ModeSocket == 0 {
		return errors.New("existing path is not a socket; refusing to remove it")
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		conn.Close()
		return errors.New("existing socket is active; refusing to replace it")
	}
	if !errors.Is(err, unix.ECONNREFUSED) {
		return fmt.Errorf("cannot confirm socket is stale: %w", err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) {
		return errors.New("socket changed during inspection; refusing to remove it")
	}
	return os.Remove(path)
}

// Register accepts already composed services. It neither opens a listener nor
// initializes PVE connections.
func Register(server *grpc.Server, controller *Controller, version string) {
	csi.RegisterIdentityServer(server, newIdentity(controller.cfg.DriverName, version,
		csi.PluginCapability_Service_CONTROLLER_SERVICE,
		csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS))
	csi.RegisterControllerServer(server, controller)
}

func RegisterNode(server *grpc.Server, node *Node, version string) {
	csi.RegisterIdentityServer(server, newIdentity(node.cfg.DriverName, version,
		csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS))
	csi.RegisterNodeServer(server, node)
}

type Identity struct {
	csi.UnimplementedIdentityServer
	name     string
	version  string
	services []csi.PluginCapability_Service_Type
}

func newIdentity(name, version string, services ...csi.PluginCapability_Service_Type) *Identity {
	return &Identity{name: name, version: version, services: append([]csi.PluginCapability_Service_Type(nil), services...)}
}

func (s *Identity) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: s.name, VendorVersion: s.version}, nil
}

func (s *Identity) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	capabilities := make([]*csi.PluginCapability, 0, len(s.services))
	for _, service := range s.services {
		capabilities = append(capabilities, &csi.PluginCapability{Type: &csi.PluginCapability_Service_{Service: &csi.PluginCapability_Service{Type: service}}})
	}
	return &csi.GetPluginCapabilitiesResponse{Capabilities: capabilities}, nil
}

func (s *Identity) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	// Production registration follows configuration and adapter startup checks.
	// This reports initialization
	// only: it does not claim PVE connectivity or provisioner readiness.
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
