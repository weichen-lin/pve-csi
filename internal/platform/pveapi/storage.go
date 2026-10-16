package pveapi

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type StorageInfo struct {
	Node    string
	Storage string
	Type    string
	Content string
	Total   int64
	Used    int64
	Avail   int64
	Enabled bool
	Active  bool
}

func (c *Client) Storage(ctx context.Context, node, storageID string) (StorageInfo, error) {
	if strings.TrimSpace(node) == "" || storageID == "" {
		return StorageInfo{}, errors.New("storage query requires a node and storage ID")
	}
	var status struct {
		Type    string `json:"type"`
		Content string `json:"content"`
		Enabled *int   `json:"enabled"`
		Active  *int   `json:"active"`
		Total   *int64 `json:"total"`
		Used    *int64 `json:"used"`
		Avail   *int64 `json:"avail"`
	}
	apiPath := fmt.Sprintf("/api2/json/nodes/%s/storage/%s/status", url.PathEscape(node), url.PathEscape(storageID))
	if err := c.GetData(ctx, apiPath, &status); err != nil {
		return StorageInfo{}, err
	}
	if strings.TrimSpace(status.Type) == "" {
		return StorageInfo{}, errors.New("PVE storage status is missing its type")
	}
	if status.Enabled == nil || status.Active == nil {
		return StorageInfo{}, errors.New("PVE storage status is missing enabled or active")
	}
	if (*status.Enabled != 0 && *status.Enabled != 1) || (*status.Active != 0 && *status.Active != 1) {
		return StorageInfo{}, errors.New("PVE storage status has an invalid enabled or active flag")
	}
	if *status.Enabled == 0 {
		return StorageInfo{}, fmt.Errorf("PVE storage %s is not enabled on node %s", storageID, node)
	}
	if *status.Active == 0 {
		return StorageInfo{}, fmt.Errorf("PVE storage %s is not active on node %s", storageID, node)
	}
	if status.Total == nil || status.Used == nil || status.Avail == nil {
		return StorageInfo{}, errors.New("PVE storage status is missing total, used or avail")
	}
	if *status.Total < 0 || *status.Used < 0 || *status.Avail < 0 {
		return StorageInfo{}, errors.New("PVE storage status contains a negative capacity")
	}
	return StorageInfo{
		Node: node, Storage: storageID, Type: status.Type, Content: status.Content,
		Total: *status.Total, Used: *status.Used, Avail: *status.Avail,
		Enabled: true, Active: true,
	}, nil
}

type ContentEntry struct {
	VolumeID string `json:"volid"`
	VMID     int    `json:"vmid"`
	Format   string `json:"format"`
	Content  string `json:"content"`
	Size     int64  `json:"size"`
}

func (c *Client) ListImages(ctx context.Context, node, storage string, owner int) ([]ContentEntry, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/storage/%s/content", url.PathEscape(node), url.PathEscape(storage))
	query := url.Values{"content": {"images"}, "vmid": {strconv.Itoa(owner)}}
	var entries []ContentEntry
	err := c.GetData(ctx, path+"?"+query.Encode(), &entries)
	return entries, err
}

func (c *Client) AllocateImage(ctx context.Context, node, storage, filename string, owner int, capacityBytes int64) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/storage/%s/content", url.PathEscape(node), url.PathEscape(storage))
	form := url.Values{"filename": {filename}, "vmid": {strconv.Itoa(owner)}, "size": {strconv.FormatInt(capacityBytes/1024, 10)}, "format": {"raw"}}
	var reference string
	err := c.PostData(ctx, path, form, &reference)
	return reference, err
}
