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

// Serve owns the newly created socket, never removes an existing one, and
// bounds shutdown so a stalled PVE request cannot keep the process alive.
func Serve(ctx context.Context, endpoint string, server *grpc.Server, out io.Writer) error {
	path, err := SocketPath(endpoint)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
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

// Register accepts already composed services. It neither opens a listener nor
// initializes PVE connections.
func Register(server *grpc.Server, controller *Controller, version string) {
	csi.RegisterIdentityServer(server, NewIdentity(controller.cfg.DriverName, version))
	csi.RegisterControllerServer(server, controller)
}

type Identity struct {
	csi.UnimplementedIdentityServer
	name    string
	version string
}

func NewIdentity(name, version string) *Identity { return &Identity{name: name, version: version} }

func (s *Identity) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: s.name, VendorVersion: s.version}, nil
}

func (s *Identity) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{Capabilities: []*csi.PluginCapability{
		{Type: &csi.PluginCapability_Service_{Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_CONTROLLER_SERVICE}}},
		{Type: &csi.PluginCapability_Service_{Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS}}},
	}}, nil
}

func (s *Identity) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	// Registration follows configuration validation. This reports initialization
	// only: it does not claim PVE connectivity or provisioner readiness.
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
