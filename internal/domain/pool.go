package domain

import "errors"

// Pool describes current capacity and adapter-supported volume formats.
type Pool struct {
	Enabled         bool
	Active          bool
	SupportsVolumes bool
	AvailableBytes  int64
}

func (p Pool) CheckReady() error {
	if !p.Enabled {
		return ErrPoolDisabled
	}
	if !p.Active {
		return ErrPoolInactive
	}
	if !p.SupportsVolumes {
		return ErrStorage
	}
	if p.AvailableBytes < 0 {
		return errors.New("storage available capacity cannot be negative")
	}
	return nil
}

func (p Pool) CheckCapacity(requestedBytes int64) error {
	if err := p.CheckReady(); err != nil {
		return err
	}
	if requestedBytes <= 0 {
		return ErrInvalidCapacity
	}
	if requestedBytes > p.AvailableBytes {
		return ErrCapacity
	}
	return nil
}
