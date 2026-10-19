package volume

import (
	"context"

	"pve-csi.local/csi-plugin/internal/domain"
)

// NodeInventory returns observed non-template VMs after read-access checks.
// Query failures must return an error, never a usable partial result.
// The operator must grant unfiltered inventory access; the API cannot prove
// that no completely hidden VM exists.
type NodeInventory interface {
	VirtualMachines(context.Context, string) ([]domain.VirtualMachine, error)
}

// resolveNode always observes current VM identities; it never caches a match.
// Publish validates and parses the raw UUID before any backend reads.
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
