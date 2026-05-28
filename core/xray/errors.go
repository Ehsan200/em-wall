package xray

import "errors"

var (
	ErrInvalidName     = errors.New("xray: invalid name (lowercase alphanumeric, -, _, max 64 chars)")
	ErrInvalidOutbound = errors.New("xray: outbound must be a JSON object containing a non-empty \"protocol\"")
	ErrNotFound        = errors.New("xray: not found")
	ErrDuplicate       = errors.New("xray: name already exists")
	ErrNoPortFree          = errors.New("xray: no free local socks port in allocator range")
	ErrInvalidRoutingRules = errors.New("xray: routing rules must be a JSON array of objects")
)
