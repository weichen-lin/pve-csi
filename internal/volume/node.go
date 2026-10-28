package volume

import (
	"context"

	"pve-csi.local/csi-plugin/internal/domain"
)

// NodeInventory requires unfiltered read access and returns no partial results.
// The API cannot detect completely hidden VMs.
type NodeInventory interface {
	VirtualMachines(context.Context, string) ([]domain.VirtualMachine, error)
}

// resolveNode rereads VM identities to detect replacements and UUID collisions.
func (s *Service) resolveNode(ctx context.Context, id domain.NodeID) (domain.VirtualMachine, error) {
	if err := ctx.Err(); err != nil {
		return domain.VirtualMachine{}, AtStep("resolve_node", s.scope.Node, err)
	}
	machines, err := s.attachments.VirtualMachines(ctx, s.scope.Node)
	if err != nil {
		return domain.VirtualMachine{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.VirtualMachine{}, AtStep("resolve_node", s.scope.Node, err)
	}
	vm, err := domain.MatchNode(s.scope.Node, id, machines)
	return vm, AtStep("resolve_node", s.scope.Node+"/"+id.String(), err)
}
