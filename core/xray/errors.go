package xray

import "errors"

var (
	ErrInvalidName         = errors.New("xray: invalid name (lowercase alphanumeric, -, _, max 64 chars)")
	ErrInvalidOutbound     = errors.New("xray: outbound must be a JSON object containing a non-empty \"protocol\"")
	ErrNotFound            = errors.New("xray: not found")
	ErrDuplicate           = errors.New("xray: name already exists")
	ErrNoPortFree          = errors.New("xray: no free local socks port in allocator range")
	ErrInvalidRoutingRules = errors.New("xray: routing rules must be a JSON array of objects")

	ErrInvalidSubURL     = errors.New("xray: subscription URL must be a non-empty http(s) URL")
	ErrEmptySubscription = errors.New("xray: subscription contained no parseable share links")

	ErrInvalidDialer = errors.New("xray: dialer must be comma-separated xray:/xraysub:/proxy: refs")
	ErrDialerCycle   = errors.New("xray: dialer must not reference its own entry")
	ErrSlotOverflow  = errors.New("xray: too many master entries with a dialer (slot pool exhausted)")

	ErrInvalidSetMember = errors.New("xray: set members must be comma-separated xray:/proxy: refs")
	ErrEmptySet         = errors.New("xray: a set must have at least one member")
	ErrSetInUse         = errors.New("xray: set is still referenced by one or more rules")
)
