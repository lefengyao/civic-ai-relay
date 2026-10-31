package relay

import "errors"

var (
	ErrUnauthorized = errors.New("client authentication failed")
	// ErrTokenExpired 表示令牌本身有效但已超过有效期（短时令牌到期）。
	// 与 ErrUnauthorized 分开，客户端才能区分「该换新令牌」和「凭据不对」。
	ErrTokenExpired              = errors.New("client token expired")
	ErrModelNotAllowed           = errors.New("model is not allowed for client key")
	ErrGlobalConcurrencyExceeded = errors.New("global concurrency limit exceeded")
	ErrKeyConcurrencyExceeded    = errors.New("client key concurrency limit exceeded")
	ErrInvalidRequest            = errors.New("invalid relay request")
	ErrSettlementUnavailable     = errors.New("settlement capacity unavailable")
)
