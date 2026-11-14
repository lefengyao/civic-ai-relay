package relay

import (
	"errors"
	"fmt"
)

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

// KeyConcurrencyError 是 ErrKeyConcurrencyExceeded 的「带数字」版本。
//
// 只看错误码的话，客户端与运维无法区分「网关整体满了」和「这把 Key 自己满了」，
// 也看不到在跑几个、上限多少——两份并发闸（全局 + Key）都会返回同一个
// rate_limit_exceeded，排查时只能猜。这里把 Key 名与两个数字带上，公共接口
// 据此拼出可读文案；Unwrap 保留哨兵错误，既有的 errors.Is 判定不受影响。
type KeyConcurrencyError struct {
	Key    string
	Active int
	Limit  int
}

func (e *KeyConcurrencyError) Error() string {
	if e == nil {
		return ErrKeyConcurrencyExceeded.Error()
	}
	return fmt.Sprintf("%s (active %d, limit %d)", ErrKeyConcurrencyExceeded.Error(), e.Active, e.Limit)
}

func (e *KeyConcurrencyError) Unwrap() error { return ErrKeyConcurrencyExceeded }

// Detail 是给客户端看的中文说明。与分组并发的文案对齐：并发是瞬时状态，
// 前面的请求一结束就恢复，不能套用额度那套「欠费 / 等窗口轮换」的处置建议。
func (e *KeyConcurrencyError) Detail() string {
	if e == nil {
		return ""
	}
	name := e.Key
	if name == "" {
		name = "未命名"
	}
	return fmt.Sprintf("Key「%s」并发已满（在跑 %d，上限 %d），请等前面的请求结束后重试", name, e.Active, e.Limit)
}
