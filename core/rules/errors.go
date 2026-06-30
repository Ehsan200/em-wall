package rules

import "errors"

var (
	ErrEmptyPattern   = errors.New("rules: empty pattern")
	ErrInvalidPattern = errors.New("rules: invalid pattern")
	ErrInvalidAction  = errors.New("rules: invalid action")
	ErrNotFound       = errors.New("rules: not found")
	ErrDuplicate      = errors.New("rules: pattern already exists")

	ErrEmptyGroupKey  = errors.New("rules: empty group key")
	ErrGroupNotFound  = errors.New("rules: custom group not found")
	ErrGroupDuplicate = errors.New("rules: custom group key already exists")
	ErrEmptyGroupName = errors.New("rules: empty group display name")
)
