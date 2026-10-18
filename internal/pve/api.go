package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
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
	if err := c.getData(ctx, apiPath, &status); err != nil {
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
	if status.Total == nil || status.Used == nil || status.Avail == nil {
		return StorageInfo{}, errors.New("PVE storage status is missing total, used or avail")
	}
	if *status.Total < 0 || *status.Used < 0 || *status.Avail < 0 {
		return StorageInfo{}, errors.New("PVE storage status contains a negative capacity")
	}
	return StorageInfo{
		Node: node, Storage: storageID, Type: status.Type, Content: status.Content,
		Total: *status.Total, Used: *status.Used, Avail: *status.Avail,
		Enabled: *status.Enabled == 1, Active: *status.Active == 1,
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
	err := c.getData(ctx, path+"?"+query.Encode(), &entries)
	return entries, err
}

func (c *Client) AllocateImage(ctx context.Context, node, storage, filename string, owner int, capacityBytes int64) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/storage/%s/content", url.PathEscape(node), url.PathEscape(storage))
	form := url.Values{"filename": {filename}, "vmid": {strconv.Itoa(owner)}, "size": {strconv.FormatInt(capacityBytes/1024, 10)}, "format": {"raw"}}
	var reference string
	err := c.postData(ctx, path, form, &reference)
	return reference, err
}

// No delay parameter: PVE returns a task ID, which must be observed to finish.
func (c *Client) DeleteImage(ctx context.Context, node, storage, reference string) (string, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/storage/%s/content/%s", url.PathEscape(node), url.PathEscape(storage), url.PathEscape(reference))
	var upid string
	err := c.requestData(ctx, http.MethodDelete, path, nil, &upid)
	return upid, err
}

type DeletionTask struct {
	UPID       string `json:"upid"`
	Type       string `json:"type"`
	ID         string `json:"id"`
	Status     string `json:"status"`
	ExitStatus string `json:"exitstatus"`
}

func validDeletionTask(upid, node, id string) bool {
	parts := strings.Split(upid, ":")
	return len(parts) == 9 && parts[0] == "UPID" && parts[1] == node &&
		parts[5] == "imgdel" && parts[6] == id && parts[7] != "" && parts[8] == ""
}

func (c *Client) DeletionStatus(ctx context.Context, node, upid, id string) (DeletionTask, error) {
	if !validDeletionTask(upid, node, id) {
		return DeletionTask{}, errors.New("PVE returned an unexpected deletion task ID")
	}
	path := fmt.Sprintf("/api2/json/nodes/%s/tasks/%s/status", url.PathEscape(node), url.PathEscape(upid))
	var task DeletionTask
	if err := c.getData(ctx, path, &task); err != nil {
		return task, err
	}
	if task.UPID != upid || task.Type != "imgdel" || task.ID != id ||
		(task.Status != "running" && task.Status != "stopped") || (task.Status == "stopped" && task.ExitStatus == "") {
		return DeletionTask{}, errors.New("PVE returned an invalid deletion task status")
	}
	return task, nil
}

// PVE task IDs identify owner@storage, not an individual disk. Conservatively
// block the whole owner/storage scope while a visible deletion is running.
func (c *Client) HasActiveDeletion(ctx context.Context, node, id string) (bool, error) {
	for start := 0; ; start += 100 {
		query := url.Values{"source": {"active"}, "typefilter": {"imgdel"}, "limit": {"100"}, "start": {strconv.Itoa(start)}}
		var tasks []DeletionTask
		path := fmt.Sprintf("/api2/json/nodes/%s/tasks?%s", url.PathEscape(node), query.Encode())
		if err := c.getData(ctx, path, &tasks); err != nil {
			return false, err
		}
		for _, task := range tasks {
			if task.Type != "imgdel" || task.ID == "" || task.Status == "" {
				return false, errors.New("PVE returned an invalid active deletion task")
			}
			if task.ID == id && task.Status == "RUNNING" {
				return true, nil
			}
		}
		if len(tasks) < 100 {
			return false, nil
		}
	}
}

// This provisioning-only driver uses a reserved owner VMID with no VM/CT.
func (c *Client) OwnerUnused(ctx context.Context, owner int) (bool, error) {
	if owner < 100 || owner > 999999999 {
		return false, errors.New("invalid owner VMID")
	}
	var next json.Number
	err := c.getData(ctx, "/api2/json/cluster/nextid?vmid="+strconv.Itoa(owner), &next)
	var httpErr *HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusBadRequest {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	id, err := next.Int64()
	if err != nil || id != int64(owner) {
		return false, errors.New("PVE returned an unexpected owner VMID")
	}
	return true, nil
}

var storageIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]*$`)

// StoragePermissions describes the authenticated token's effective privileges,
// including the restrictions imposed by its backing user and privilege separation.
type StoragePermissions struct {
	Audit         bool
	AllocateSpace bool
	Allocate      bool
}

func (c *Client) StoragePermissions(ctx context.Context, storageID string) (StoragePermissions, error) {
	if !storageIDPattern.MatchString(storageID) {
		return StoragePermissions{}, errors.New("permissions query requires a valid storage ID")
	}
	path := "/storage/" + storageID
	privileges, err := c.permissions(ctx, path)
	if err != nil {
		return StoragePermissions{}, err
	}
	_, audit := privileges["Datastore.Audit"]
	_, space := privileges["Datastore.AllocateSpace"]
	_, allocate := privileges["Datastore.Allocate"]
	return StoragePermissions{Audit: audit, AllocateSpace: space, Allocate: allocate}, nil
}

func (c *Client) CanConfigureDisk(ctx context.Context, owner int) (bool, error) {
	if owner < 100 || owner > 999999999 {
		return false, errors.New("invalid disk owner VMID")
	}
	privileges, err := c.permissions(ctx, "/vms/"+strconv.Itoa(owner))
	if err != nil {
		return false, err
	}
	_, allowed := privileges["VM.Config.Disk"]
	return allowed, nil
}

func (c *Client) permissions(ctx context.Context, path string) (map[string]*int, error) {
	query := url.Values{"path": {path}}
	var paths map[string]map[string]*int
	if err := c.getData(ctx, "/api2/json/access/permissions?"+query.Encode(), &paths); err != nil {
		return nil, err
	}
	privileges, found := paths[path]
	if found && privileges == nil {
		return nil, errors.New("PVE permissions must be an object")
	}
	for _, propagate := range privileges {
		if propagate == nil || (*propagate != 0 && *propagate != 1) {
			return nil, errors.New("PVE permissions contain an invalid propagation flag")
		}
	}
	// The value is propagation, not a grant boolean. A present privilege with
	// value 0 still grants access at this exact path. Never infer parent grants.
	return privileges, nil
}
