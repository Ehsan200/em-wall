package proxy

import "errors"

var (
	ErrInvalidName     = errors.New("proxy: invalid name (lowercase alphanumeric, -, _, max 64 chars)")
	ErrInvalidProtocol = errors.New("proxy: invalid protocol (want socks5 or http)")
	ErrInvalidHost     = errors.New("proxy: host is required")
	ErrInvalidPort     = errors.New("proxy: port must be 1..65535")
	ErrNotFound        = errors.New("proxy: not found")
	ErrDuplicate       = errors.New("proxy: name already exists")
)
