package pveapi

import (
	"fmt"
	"net/http"
)

// HTTPError preserves status classification without retaining a server body
// or credentials. The query CLI keeps the same human-readable error format.
type HTTPError struct {
	Method     string
	Path       string
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("PVE %s %s: HTTP %d (%s)", e.Method, e.Path, e.StatusCode, http.StatusText(e.StatusCode))
}
