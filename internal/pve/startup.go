package pve

import (
	"context"
	"errors"
	"net/url"

	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

// CheckAccess validates node visibility and inventory/task read permissions.
// Write permissions are checked for each CSI operation.
func (c *Client) CheckAccess(ctx context.Context, node string) error {
	if !domain.ValidNodeName(node) {
		return domain.ErrInvalidInventory
	}
	if err := c.checkTaskAccess(ctx, node); err != nil {
		return err
	}
	inventory, err := NewNodeInventory(c)
	if err != nil {
		return err
	}
	if err := inventory.checkInventoryAccess(ctx); err != nil {
		return err
	}
	// ACL paths may exist without a node; verify the node itself responds.
	var state struct {
		Uptime *int64 `json:"uptime"`
	}
	path := "/api2/json/nodes/" + url.PathEscape(node) + "/status"
	if err := c.getData(ctx, path, &state); err != nil {
		return volume.AtStep("check_pve_node", node, mapError(err))
	}
	if state.Uptime == nil || *state.Uptime < 0 {
		return volume.AtStep("check_pve_node", node, mapError(errors.New("PVE node status has no valid uptime")))
	}
	return ctx.Err()
}
