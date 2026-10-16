package csiserver

import (
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
)

// Register accepts already composed services. It neither opens a listener nor
// initializes PVE connections, and never changes the query CLI's behavior.
func Register(server *grpc.Server, controller csi.ControllerServer, version string) {
	csi.RegisterIdentityServer(server, NewIdentity(version))
	csi.RegisterControllerServer(server, controller)
}
