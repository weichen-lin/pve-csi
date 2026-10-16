package domain

import "math"

const GiB int64 = 1 << 30

// NormalizeCapacity converts an optional capacity range into whole-GiB
// allocation bounds. specified distinguishes an omitted range from empty input.
func NormalizeCapacity(required, upper int64, specified bool) (int64, int64, error) {
	limit := int64(math.MaxInt64)
	if specified {
		if required < 0 || upper < 0 || (required == 0 && upper == 0) || (upper > 0 && required > upper) {
			return 0, 0, ErrInvalidCapacity
		}
		if upper != 0 {
			limit = upper
		}
	} else {
		required = 0
	}
	if required < GiB {
		required = GiB
	}
	if remainder := required % GiB; remainder != 0 {
		increment := GiB - remainder
		if required > math.MaxInt64-increment {
			return 0, 0, ErrCapacityOverflow
		}
		required += increment
	}
	if required > limit {
		return 0, 0, ErrCapacityOutOfRange
	}
	return required, limit, nil
}
