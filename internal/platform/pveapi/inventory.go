package pveapi

import (
	"context"
	"errors"
	"strings"
)

func (c *Client) Version(ctx context.Context) (VersionInfo, error) {
	var info VersionInfo
	if err := c.GetData(ctx, "/api2/json/version", &info); err != nil {
		return VersionInfo{}, err
	}
	if strings.TrimSpace(info.Version) == "" {
		return VersionInfo{}, errors.New("PVE version response is missing data.version")
	}
	return info, nil
}

func (c *Client) Resources(ctx context.Context) ([]Resource, error) {
	var resources []Resource
	if err := c.GetData(ctx, "/api2/json/cluster/resources", &resources); err != nil {
		return nil, err
	}
	for _, entry := range resources {
		if entry.ID == "" || entry.Type == "" {
			return nil, errors.New("PVE resource response is missing an id or type")
		}
	}
	return resources, nil
}
