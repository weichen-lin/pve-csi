package pve

import (
	"context"
	"errors"
	"net/url"

	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

// CheckAccess verifies PVE access and node existence before CSI is served.
// Pool and target-VM write permissions depend on later CSI requests. Do not
// demand permissions on unrelated pools/VMs or cache this as authorization.
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
	// Permission paths can exist independently of the node. Verify the actual
	// configured node responds, using the Sys.Audit grant checked above.
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
