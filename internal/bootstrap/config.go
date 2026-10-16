package bootstrap

import (
	"errors"
	"pve-csi.local/csi-plugin/internal/platform/pveapi"
	"pve-csi.local/csi-plugin/internal/volume/domain"
	"regexp"
	"strconv"
	"strings"
)

type Config struct {
	Connection pveapi.Config
	Query      QueryConfig
}

type QueryConfig struct {
	VM      pveapi.VMQuery
	Storage string
}

var vmIDPattern = regexp.MustCompile(`^[1-9][0-9]{2,8}$`)

func LoadConfig(getenv func(string) string) (Config, error) {
	connection, err := (pveapi.Config{
		Endpoint: getenv("PVE_ENDPOINT"), TokenID: getenv("PVE_TOKEN_ID"),
		TokenSecret: getenv("PVE_TOKEN_SECRET"), CAFile: getenv("PVE_CA_FILE"),
	}).Normalize()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Connection: connection, Query: QueryConfig{
		Storage: strings.TrimSpace(getenv("PVE_STORAGE")),
		VM:      pveapi.VMQuery{ExpectedUUID: strings.TrimSpace(getenv("PVE_EXPECTED_VM_UUID")), ExpectedName: strings.TrimSpace(getenv("PVE_EXPECTED_VM_NAME"))},
	}}
	if rawVMID := strings.TrimSpace(getenv("PVE_VM_ID")); rawVMID != "" {
		if !vmIDPattern.MatchString(rawVMID) {
			return Config{}, errors.New("PVE_VM_ID must be an integer from 100 to 999999999 without leading zeros")
		}
		cfg.Query.VM.VMID, err = strconv.Atoi(rawVMID)
		if err != nil {
			return Config{}, errors.New("PVE_VM_ID could not be parsed")
		}
	}
	if cfg.Query.Storage != "" {
		if !domain.ValidStorageID(cfg.Query.Storage) {
			return Config{}, errors.New("PVE_STORAGE must be a valid PVE storage ID")
		}
		if cfg.Query.VM.VMID == 0 {
			return Config{}, errors.New("PVE_VM_ID is required when PVE_STORAGE is set")
		}
	}
	if cfg.Query.VM.ExpectedUUID != "" {
		if !pveapi.ValidUUID(cfg.Query.VM.ExpectedUUID) {
			return Config{}, errors.New("PVE_EXPECTED_VM_UUID must have the hexadecimal 8-4-4-4-12 format")
		}
		cfg.Query.VM.ExpectedUUID = strings.ToLower(cfg.Query.VM.ExpectedUUID)
	}
	if strings.ContainsAny(cfg.Query.VM.ExpectedName, "\r\n") {
		return Config{}, errors.New("PVE_EXPECTED_VM_NAME must not contain line breaks")
	}
	if (cfg.Query.VM.ExpectedUUID != "" || cfg.Query.VM.ExpectedName != "") && cfg.Query.VM.VMID == 0 {
		return Config{}, errors.New("PVE_VM_ID is required when PVE_EXPECTED_VM_UUID or PVE_EXPECTED_VM_NAME is set")
	}
	return cfg, nil
}
