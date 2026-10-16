package domain

import "errors"

var (
	ErrConflict           = errors.New("existing volume conflicts with requested owner, format, content or capacity")
	ErrInvalidCapacity    = errors.New("capacity range must contain positive bounds in ascending order")
	ErrCapacityOutOfRange = errors.New("capacity range cannot fit a whole-GiB volume of at least 1 GiB")
	ErrCapacityOverflow   = errors.New("capacity cannot be rounded to whole GiB without overflow")
)
