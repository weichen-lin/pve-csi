package bootstrap

import (
	"google.golang.org/grpc"
	"pve-csi.local/csi-plugin/internal/platform/csiserver"
	"pve-csi.local/csi-plugin/internal/platform/pveapi"
	"pve-csi.local/csi-plugin/internal/volume/application"
	"pve-csi.local/csi-plugin/internal/volume/infrastructure/pve"
	csidriver "pve-csi.local/csi-plugin/internal/volume/interfaces/csi"
)

// NewController composes one shared coordinator per Controller instance.
// Query VM settings are not accepted here; allocation ownership is explicit.
func NewController(cfg csidriver.Config, client *pveapi.Client) (*csidriver.Controller, error) {
	gateway, err := pve.NewStorageGateway(client)
	if err != nil {
		return nil, err
	}
	service, err := application.NewService(gateway)
	if err != nil {
		return nil, err
	}
	coordinated, err := application.NewCoordinator(service)
	if err != nil {
		return nil, err
	}
	return csidriver.NewController(cfg, coordinated)
}

func RegisterController(server *grpc.Server, cfg csidriver.Config, client *pveapi.Client) error {
	controller, err := NewController(cfg, client)
	if err != nil {
		return err
	}
	csiserver.Register(server, controller, Version)
	return nil
}
