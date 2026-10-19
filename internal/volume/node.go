package volume

import (
	"context"
	"errors"

	"pve-csi.local/csi-plugin/internal/domain"
)

// NodeInventory returns observed non-template VMs after read-access checks.
// Query failures must return an error, never a usable partial result.
// The operator must grant unfiltered inventory access; the API cannot prove
// that no completely hidden VM exists.
type NodeInventory interface {
	VirtualMachines(context.Context, string) ([]domain.VirtualMachine, error)
}

type NodeResolver struct {
	inventory NodeInventory
	node      string
}

func NewNodeResolver(inventory NodeInventory, node string) (*NodeResolver, error) {
	if inventory == nil || !domain.ValidNodeName(node) {
		return nil, errors.New("node resolver requires an inventory and valid PVE node")
	}
	return &NodeResolver{inventory: inventory, node: node}, nil
}

// Resolve observes identity afresh. It does not authorize attachment or lock a VM.
func (r *NodeResolver) Resolve(ctx context.Context, rawID string) (domain.VirtualMachine, error) {
	if err := ctx.Err(); err != nil {
		return domain.VirtualMachine{}, AtStep("resolve_node", r.node, err)
	}
	id, err := domain.ParseNodeID(rawID)
	if err != nil {
		return domain.VirtualMachine{}, AtStep("validate_node_id", "node", err)
	}
	machines, err := r.inventory.VirtualMachines(ctx, r.node)
	if err != nil {
		return domain.VirtualMachine{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.VirtualMachine{}, AtStep("resolve_node", r.node, err)
	}
	vm, err := domain.MatchNode(r.node, id, machines)
	return vm, AtStep("resolve_node", r.node+"/"+id.String(), err)
}
