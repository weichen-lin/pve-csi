package pve

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"pve-csi.local/csi-plugin/internal/domain"
)

// Absent identity is a non-candidate during discovery, not an unreadable VM.
var errMissingVMUUID = errors.New("PVE VM has no SMBIOS UUID")

var driveSlot = regexp.MustCompile(`^(scsi|sata|virtio|ide|unused|mp)[0-9]+$|^(rootfs|efidisk0|tpmstate0)$`)

// vmConfig translates PVE's dynamic drive keys at the adapter boundary.
// Missing optional settings use PVE defaults; malformed settings never do.
type vmConfig struct {
	Digest, SMBIOS, Lock, Hotplug, SCSIHardware string
	Template                                    int
	Drives                                      map[string]string
}

func (cfg *vmConfig) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return domain.ErrInvalidInventory
	}
	parsed := vmConfig{Drives: make(map[string]string)}
	for key, target := range map[string]*string{
		"digest": &parsed.Digest, "smbios1": &parsed.SMBIOS, "lock": &parsed.Lock,
		"hotplug": &parsed.Hotplug, "scsihw": &parsed.SCSIHardware,
	} {
		if raw, exists := fields[key]; exists {
			value, err := configString(raw)
			if err != nil {
				return err
			}
			*target = value
		}
	}
	if raw, exists := fields["template"]; exists {
		if string(raw) == "null" || json.Unmarshal(raw, &parsed.Template) != nil || (parsed.Template != 0 && parsed.Template != 1) {
			return domain.ErrInvalidInventory
		}
	}
	for key, raw := range fields {
		if !driveSlot.MatchString(key) {
			continue
		}
		value, err := configString(raw)
		if err != nil || value == "" {
			return domain.ErrInvalidInventory
		}
		parsed.Drives[key] = value
	}
	*cfg = parsed
	return nil
}

func configString(raw json.RawMessage) (string, error) {
	var value string
	if string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
		return "", domain.ErrInvalidInventory
	}
	return value, nil
}

func (cfg vmConfig) nodeID() (domain.NodeID, error) {
	var uuid string
	for _, field := range strings.Split(cfg.SMBIOS, ",") {
		if value, ok := strings.CutPrefix(field, "uuid="); ok {
			if uuid != "" || value == "" {
				return domain.NodeID{}, domain.ErrInvalidInventory
			}
			uuid = value
		}
	}
	if uuid == "" {
		return domain.NodeID{}, errMissingVMUUID
	}
	id, err := domain.ParseNodeID(uuid)
	if err != nil {
		return domain.NodeID{}, domain.ErrInvalidInventory
	}
	return id, nil
}
