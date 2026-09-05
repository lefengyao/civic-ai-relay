package relay

import "errors"

var (
	ErrUnauthorized              = errors.New("client authentication failed")
	ErrModelNotAllowed           = errors.New("model is not allowed for client key")
	ErrGlobalConcurrencyExceeded = errors.New("global concurrency limit exceeded")
	ErrKeyConcurrencyExceeded    = errors.New("client key concurrency limit exceeded")
	ErrInvalidRequest            = errors.New("invalid relay request")
)
