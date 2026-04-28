package rules

import "errors"

var (
	ErrEmptyPattern   = errors.New("rules: empty pattern")
	ErrInvalidPattern = errors.New("rules: invalid pattern")
	ErrInvalidAction  = errors.New("rules: invalid action")
	ErrNotFound       = errors.New("rules: not found")
	ErrDuplicate      = errors.New("rules: pattern already exists")
)
