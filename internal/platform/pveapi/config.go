package pveapi

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Config contains connection settings only, never a queried VM or allocation owner.
type Config struct {
	Endpoint    string
	TokenID     string
	TokenSecret string
	CAFile      string
}

func (cfg Config) Normalize() (Config, error) {
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	cfg.TokenID = strings.TrimSpace(cfg.TokenID)
	cfg.CAFile = strings.TrimSpace(cfg.CAFile)
	for _, required := range []struct{ name, value string }{
		{"PVE_ENDPOINT", cfg.Endpoint}, {"PVE_TOKEN_ID", cfg.TokenID}, {"PVE_TOKEN_SECRET", cfg.TokenSecret},
	} {
		if required.value == "" {
			return Config{}, fmt.Errorf("%s is required", required.name)
		}
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return Config{}, errors.New("PVE_ENDPOINT must be an HTTPS URL with a host")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return Config{}, errors.New("PVE_ENDPOINT must contain only the HTTPS host and optional port")
	}
	if strings.ContainsAny(cfg.TokenID, "\r\n") || strings.ContainsAny(cfg.TokenSecret, "\r\n") {
		return Config{}, errors.New("PVE_TOKEN_ID and PVE_TOKEN_SECRET must not contain line breaks")
	}
	cfg.Endpoint = u.Scheme + "://" + u.Host
	return cfg, nil
}
