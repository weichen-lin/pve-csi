package application

import (
	"context"
	"errors"
	"sync"

	"pve-csi.local/csi-plugin/internal/volume/domain"
)

// Coordinator must be created once and shared by the Controller handlers that
// belong to one instance. It does not coordinate other processes or PVE clients.
type Coordinator struct {
	next     Provisioner
	mu       sync.Mutex
	inFlight map[string]struct{}
}

func NewCoordinator(next Provisioner) (*Coordinator, error) {
	if next == nil {
		return nil, errors.New("coordination requires a provisioner")
	}
	return &Coordinator{next: next, inFlight: make(map[string]struct{})}, nil
}

func (c *Coordinator) EnsureVolume(ctx context.Context, req domain.Request) (domain.Volume, error) {
	if err := ctx.Err(); err != nil {
		return domain.Volume{}, err
	}
	if err := req.Validate(); err != nil {
		return domain.Volume{}, err
	}
	c.mu.Lock()
	_, busy := c.inFlight[req.Name]
	if !busy {
		c.inFlight[req.Name] = struct{}{}
	}
	c.mu.Unlock()
	if busy {
		return domain.Volume{}, ErrBusy
	}
	defer func() {
		c.mu.Lock()
		delete(c.inFlight, req.Name)
		c.mu.Unlock()
	}()
	return c.next.EnsureVolume(ctx, req)
}
