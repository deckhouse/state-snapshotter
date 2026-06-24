package restore

import "errors"

var (
	ErrNotFound          = errors.New("not found")
	ErrNotReady          = errors.New("not ready")
	ErrContractViolation = errors.New("contract violation")
	ErrBadRequest        = errors.New("bad request")
)
