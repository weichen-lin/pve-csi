package pveapi

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

func ValidUUID(value string) bool { return uuidPattern.MatchString(value) }

func FindVM(resources []Resource, vmID int) (Resource, error) {
	var selected Resource
	matches := 0
	for _, entry := range resources {
		if entry.Type == "qemu" && entry.VMID == vmID {
			selected = entry
			matches++
		}
	}
	if matches == 0 {
		return Resource{}, fmt.Errorf("PVE VM %d not found or not visible to this Token", vmID)
	}
	if matches != 1 {
		return Resource{}, fmt.Errorf("multiple PVE resources match VM %d", vmID)
	}
	if selected.ID != fmt.Sprintf("qemu/%d", vmID) {
		return Resource{}, fmt.Errorf("PVE VM %d resource has an inconsistent id", vmID)
	}
	if strings.TrimSpace(selected.Node) == "" {
		return Resource{}, fmt.Errorf("PVE VM %d resource is missing its node", vmID)
	}
	if selected.Template != 0 {
		return Resource{}, fmt.Errorf("PVE VM %d is a template, not a worker VM", vmID)
	}
	return selected, nil
}

func (c *Client) QueryVM(ctx context.Context, query VMQuery, resources []Resource) (VMInfo, error) {
	selected, err := FindVM(resources, query.VMID)
	if err != nil {
		return VMInfo{}, err
	}
	var details struct {
		Name     string `json:"name"`
		SMBIOS1  string `json:"smbios1"`
		Template int    `json:"template"`
	}
	// current=1 excludes pending changes when reading the VM's configured identity.
	apiPath := fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/config?current=1", url.PathEscape(selected.Node), query.VMID)
	if err := c.GetData(ctx, apiPath, &details); err != nil {
		return VMInfo{}, err
	}
	if details.Template != 0 {
		return VMInfo{}, fmt.Errorf("PVE VM %d configuration is a template, not a worker VM", query.VMID)
	}
	uuid, err := SMBIOSUUID(details.SMBIOS1)
	if err != nil {
		return VMInfo{}, fmt.Errorf("PVE VM %d: %w", query.VMID, err)
	}
	if query.ExpectedUUID != "" && uuid != query.ExpectedUUID {
		return VMInfo{}, fmt.Errorf("PVE VM %d UUID mismatch: expected %s, got %s", query.VMID, query.ExpectedUUID, uuid)
	}
	if query.ExpectedName != "" && details.Name != query.ExpectedName {
		return VMInfo{}, fmt.Errorf("PVE VM %d name mismatch: expected %q, got %q", query.VMID, query.ExpectedName, details.Name)
	}
	// The Resource summary is a display fallback, not proof of the current name.
	name := details.Name
	if name == "" {
		name = selected.Name
	}
	return VMInfo{VMID: query.VMID, Node: selected.Node, Name: name, UUID: uuid}, nil
}

func SMBIOSUUID(smbios string) (string, error) {
	uuid := ""
	found := false
	for _, property := range strings.Split(smbios, ",") {
		key, value, ok := strings.Cut(property, "=")
		if !ok || strings.TrimSpace(key) != "uuid" {
			continue
		}
		if found {
			return "", errors.New("SMBIOS configuration contains duplicate uuid properties")
		}
		found = true
		uuid = strings.TrimSpace(value)
	}
	if !found {
		return "", errors.New("SMBIOS configuration is missing uuid")
	}
	if !uuidPattern.MatchString(uuid) {
		return "", errors.New("SMBIOS uuid must have the hexadecimal 8-4-4-4-12 format")
	}
	return strings.ToLower(uuid), nil
}
