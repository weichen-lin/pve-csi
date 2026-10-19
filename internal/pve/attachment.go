package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/volume"
)

type AttachmentGateway struct{ *NodeInventory }

func NewAttachmentGateway(client *Client) (*AttachmentGateway, error) {
	inventory, err := NewNodeInventory(client)
	if err != nil {
		return nil, err
	}
	return &AttachmentGateway{inventory}, nil
}

var scsiSlot = regexp.MustCompile(`^scsi([1-9]|[12][0-9]|30)$`)

func (g *AttachmentGateway) config(ctx context.Context, path string) (vmConfig, error) {
	var cfg vmConfig
	if err := g.client.getData(ctx, path, &cfg); err != nil {
		return vmConfig{}, mapError(err)
	}
	if cfg.Digest == "" {
		return vmConfig{}, domain.ErrInvalidInventory
	}
	return cfg, nil
}

func referencesIn(cfg vmConfig, id domain.Identity, source domain.ReferenceSource, state domain.ReferenceState) (domain.References, error) {
	var refs domain.References
	for key, value := range cfg.Drives {
		parts := strings.Split(value, ",")
		reference := strings.TrimPrefix(parts[0], "file=")
		reference = strings.TrimPrefix(reference, "volume=")
		if reference != id.Reference() {
			continue
		}
		serial := ""
		seenSerial := false
		for _, part := range parts[1:] {
			if v, ok := strings.CutPrefix(part, "serial="); ok {
				if seenSerial || v == "" {
					return nil, domain.ErrInvalidInventory
				}
				seenSerial = true
				serial = v
			}
		}
		kind := state
		if strings.HasPrefix(key, "unused") {
			kind = domain.ReferenceUnused
		}
		refs = append(refs, domain.DiskReference{Source: source, Slot: key, Serial: serial, State: kind})
	}
	return refs, nil
}

// References reads current, pending and snapshot configurations, including
// templates and containers. A failed/filtered read never proves absence.
func (g *AttachmentGateway) References(ctx context.Context, id domain.Identity) (domain.References, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	if err := g.checkInventoryAccess(ctx); err != nil {
		return nil, err
	}
	// Do not infer absence while a timed-out attach/detach worker can still
	// change the VM configuration. This also protects deletion after a restart.
	active, err := g.client.activeTasks(ctx, id.Placement().Node, url.Values{"source": {"active"}, "typefilter": {"qmconfig"}})
	if err != nil {
		return nil, mapError(err)
	}
	if len(active) != 0 {
		return nil, volume.ErrBusy
	}
	var result domain.References
	for _, kind := range []string{"qemu", "lxc"} {
		entries, err := g.client.listGuests(ctx, id.Placement().Node, kind)
		if err != nil {
			return nil, mapError(err)
		}
		for _, entry := range entries {
			refs, err := g.guestReferences(ctx, id, kind, entry.VMID)
			if err != nil {
				return nil, err
			}
			result = append(result, refs...)
		}
	}
	if err := g.checkInventoryAccess(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// guestReferences completes one guest observation before it can be merged.
// A failed read returns no partial references, and pending changes affect only
// this guest's slots.
func (g *AttachmentGateway) guestReferences(ctx context.Context, id domain.Identity, kind string, vmid int) (domain.References, error) {
	if err := g.client.checkVMReadAccess(ctx, vmid); err != nil {
		return nil, err
	}
	path := "/api2/json/nodes/" + url.PathEscape(id.Placement().Node) + "/" + kind + "/" + strconv.Itoa(vmid)
	current, err := g.config(ctx, path+"/config?current=1")
	if err != nil {
		return nil, err
	}
	source := domain.ReferenceSource{Kind: domain.SourceVM, Node: id.Placement().Node, VMID: vmid}
	if kind == "lxc" {
		source.Kind = domain.SourceContainer
	} else if current.Template != 0 {
		source.Kind = domain.SourceTemplate
	}
	refs, err := referencesIn(current, id, source, domain.ReferenceAttached)
	if err != nil {
		return nil, err
	}
	// Pending API includes deleted settings as well as new values.
	var pending []struct {
		Key     string          `json:"key"`
		Pending json.RawMessage `json:"pending"`
		Delete  int             `json:"delete"`
	}
	if err := g.client.getData(ctx, path+"/pending", &pending); err != nil {
		return nil, mapError(err)
	}
	for _, p := range pending {
		if !driveSlot.MatchString(p.Key) {
			continue
		}
		if len(p.Pending) > 0 && string(p.Pending) != "null" {
			value, err := configString(p.Pending)
			if err != nil || value == "" {
				return nil, domain.ErrInvalidInventory
			}
			more, err := referencesIn(vmConfig{Drives: map[string]string{p.Key: value}}, id, source, domain.ReferencePending)
			if err != nil {
				return nil, err
			}
			refs = append(refs, more...)
		}
		if p.Delete != 0 {
			for i := range refs {
				if refs[i].Slot == p.Key {
					refs[i].State = domain.ReferencePending
				}
			}
		}
	}
	var snapshots []struct {
		Name string `json:"name"`
	}
	if err := g.client.getData(ctx, path+"/snapshot", &snapshots); err != nil {
		return nil, mapError(err)
	}
	for _, snap := range snapshots {
		if snap.Name == "current" {
			continue
		}
		if snap.Name == "" {
			return nil, domain.ErrInvalidInventory
		}
		var cfg vmConfig
		if err := g.client.getData(ctx, path+"/snapshot/"+url.PathEscape(snap.Name)+"/config", &cfg); err != nil {
			return nil, mapError(err)
		}
		more, err := referencesIn(cfg, id, source, domain.ReferenceSnapshot)
		if err != nil {
			return nil, err
		}
		refs = append(refs, more...)
	}
	// Unrelated VMs/templates need no guest UUID to prove absence.
	// References from containers/templates still block deletion without one.
	if kind == "qemu" && len(refs) != 0 && (current.Template == 0 || current.SMBIOS != "") {
		source.NodeID, err = current.nodeID()
		if err != nil {
			return nil, err
		}
		for i := range refs {
			refs[i].Source = source
		}
	}
	return refs, nil
}

func (g *AttachmentGateway) prepare(ctx context.Context, vm domain.VirtualMachine) (vmConfig, string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d", url.PathEscape(vm.Node), vm.VMID)
	allowed, err := g.client.CanConfigureDisk(ctx, vm.VMID)
	if err != nil {
		return vmConfig{}, "", mapError(err)
	}
	if !allowed {
		return vmConfig{}, "", missingPrivilege("/vms/"+strconv.Itoa(vm.VMID), "VM.Config.Disk")
	}
	// A previous RPC can time out while its PVE worker is still executing.
	active, err := g.client.activeTasks(ctx, vm.Node, url.Values{"source": {"active"}, "typefilter": {"qmconfig"}, "vmid": {strconv.Itoa(vm.VMID)}})
	if err != nil {
		return vmConfig{}, "", mapError(err)
	}
	if len(active) != 0 {
		return vmConfig{}, "", volume.ErrBusy
	}
	var pending []struct {
		Pending json.RawMessage `json:"pending"`
		Delete  int             `json:"delete"`
	}
	if err := g.client.getData(ctx, path+"/pending", &pending); err != nil {
		return vmConfig{}, "", mapError(err)
	}
	for _, p := range pending {
		if (len(p.Pending) > 0 && string(p.Pending) != "null") || p.Delete != 0 {
			return vmConfig{}, "", volume.NewBackendError(volume.FailurePrecondition, "VM has pending changes; apply or revert them in PVE before retrying", nil)
		}
	}
	cfg, err := g.config(ctx, path+"/config?current=1")
	if err != nil {
		return vmConfig{}, "", err
	}
	uuid, err := cfg.nodeID()
	if err != nil {
		return vmConfig{}, "", err
	}
	if uuid != vm.ID {
		return vmConfig{}, "", domain.ErrAttachmentConflict
	}
	if cfg.Lock != "" {
		return vmConfig{}, "", volume.ErrBusy
	}
	if cfg.Template != 0 {
		return vmConfig{}, "", domain.ErrAttachmentConflict
	}
	hotplug := cfg.Hotplug
	if hotplug != "" && !strings.Contains(","+hotplug+",", ",disk,") && hotplug != "1" {
		return vmConfig{}, "", volume.NewBackendError(volume.FailurePrecondition, "enable disk hotplug on the Kubernetes VM before retrying", nil)
	}
	return cfg, path, nil
}

func (g *AttachmentGateway) Attach(ctx context.Context, id domain.Identity, vm domain.VirtualMachine) error {
	if err := id.CheckAttachmentTarget(vm); err != nil {
		return err
	}
	cfg, path, err := g.prepare(ctx, vm)
	if err != nil {
		return volume.AtStep("prepare_attachment", id.Reference(), err)
	}
	if !strings.HasPrefix(cfg.SCSIHardware, "virtio-scsi") {
		return volume.NewBackendError(volume.FailurePrecondition, "Kubernetes VM requires a VirtIO SCSI controller", nil)
	}
	for i := 1; i <= 30; i++ {
		slot := "scsi" + strconv.Itoa(i)
		if _, used := cfg.Drives[slot]; used {
			continue
		}
		// Foreign-owner volume: never rename or transfer ownership to the node VM.
		form := url.Values{"digest": {cfg.Digest}, slot: {id.Reference() + ",serial=" + id.Serial() + ",backup=0"}}
		return g.update(ctx, path, vm, form)
	}
	return volume.NewBackendError(volume.FailurePrecondition, "VM has no free SCSI disk slot", nil)
}

func (g *AttachmentGateway) Detach(ctx context.Context, id domain.Identity, ref domain.DiskReference) error {
	if !scsiSlot.MatchString(ref.Slot) {
		return domain.ErrAttachmentConflict
	}
	vm, err := ref.DetachableVM(id)
	if err != nil {
		return err
	}
	cfg, path, err := g.prepare(ctx, vm)
	if err != nil {
		return volume.AtStep("prepare_detachment", id.Reference(), err)
	}
	refs, err := referencesIn(cfg, id, vm.ReferenceSource(), domain.ReferenceAttached)
	if err != nil {
		return err
	}
	current, found, err := refs.DetachTarget(id, domain.NodeID{})
	if err != nil || !found || current != ref {
		return domain.ErrAttachmentConflict
	}
	// No force and never delete an unusedN entry: both can mean physical deletion.
	return g.update(ctx, path, vm, url.Values{"digest": {cfg.Digest}, "delete": {ref.Slot}})
}

func (g *AttachmentGateway) update(ctx context.Context, path string, vm domain.VirtualMachine, form url.Values) error {
	var task string
	if err := g.client.postData(ctx, path+"/config", form, &task); err != nil {
		mapped := mapError(err)
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && (httpErr.StatusCode == 400 || httpErr.StatusCode == 401 || httpErr.StatusCode == 403 || httpErr.StatusCode == 404) {
			return mapped
		}
		return errors.Join(volume.ErrAttachmentUnconfirmed, mapped)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	exit, err := g.client.waitTask(waitCtx, vm.Node, task, "qmconfig", strconv.Itoa(vm.VMID))
	if err != nil {
		return errors.Join(volume.ErrAttachmentUnconfirmed, mapError(err))
	}
	if exit != "OK" {
		return volume.NewBackendError(volume.FailurePrecondition, "PVE VM configuration task failed; inspect PVE Tasks", nil)
	}
	return nil
}
