package main

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	csidriver "pve-csi.local/csi-plugin/internal/csi"
	"pve-csi.local/csi-plugin/internal/domain"
	"pve-csi.local/csi-plugin/internal/pve"
)

type Config struct {
	Connection pve.Config
	Controller csidriver.Config
}

var ownerVMIDPattern = regexp.MustCompile(`^[1-9][0-9]{2,8}$`)

func LoadConfig(getenv func(string) string) (Config, error) {
	connection, err := (pve.Config{
		Endpoint: getenv("PVE_ENDPOINT"), TokenID: getenv("PVE_TOKEN_ID"),
		TokenSecret: getenv("PVE_TOKEN_SECRET"), CAFile: getenv("PVE_CA_FILE"),
	}).Normalize()
	if err != nil {
		return Config{}, err
	}
	owner := strings.TrimSpace(getenv("PVE_OWNER_VMID"))
	if !ownerVMIDPattern.MatchString(owner) {
		return Config{}, errors.New("PVE_OWNER_VMID is required and must be an integer from 100 to 999999999 without leading zeros")
	}
	vmid, err := strconv.Atoi(owner)
	if err != nil {
		return Config{}, errors.New("PVE_OWNER_VMID could not be parsed")
	}
	controller := csidriver.Config{Node: strings.TrimSpace(getenv("PVE_NODE")), OwnerVMID: vmid}
	if !domain.ValidNodeName(controller.Node) {
		return Config{}, errors.New("PVE_NODE must be a valid PVE node name")
	}
	return Config{Connection: connection, Controller: controller}, nil
}
