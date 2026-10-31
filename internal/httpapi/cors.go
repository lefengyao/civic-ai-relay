package httpapi

import (
	"net/http"
	"strings"

	"civic-ai-relay/internal/config"
)

const corsMaxAgeSeconds = "600"

// withCORS 为整台服务器（公开接口与管理台）补上浏览器跨域支持。
//
// 放行策略来自 CORS_ALLOWED_ORIGINS（settingsFn 逐请求读取，管理台改完
// 立即生效；settingsFn 为 nil 时按 "*" 处理）：
//
//   - "*"：Access-Control-Allow-Origin 固定回 "*"，任何网页都能调用；
//   - "off"：不下发任何 CORS 头，浏览器按同源策略自行拦截；
//   - 逗号分隔的来源白名单：仅当请求 Origin 精确匹配（scheme://host[:port]，
//     大小写不敏感）时回显该 Origin，其余来源不下发 ACAO，浏览器拦截。
//
// 认证走 Authorization / X-Admin-Key / X-App-Id 请求头而非 Cookie，
// 因此不需要 Access-Control-Allow-Credentials，"*" 也不会放大 Cookie 类风险；
// 真正的访问控制仍由密钥与 Key 本身的额度、并发限制承担。
//
// 预检（OPTIONS + Access-Control-Request-Method）在放行的来源上直接以
// 204 应答，不进入业务处理器；来源不在白名单上时原样交给下游，让浏览器
// 因缺少 ACAO 而拦截。
func withCORS(next http.Handler, settingsFn func() config.Settings) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		policy := "*"
		if settingsFn != nil {
			policy = settingsFn().CORSAllowedOrigins
		}
		if strings.EqualFold(policy, "off") {
			next.ServeHTTP(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		allowValue := allowOriginValue(policy, origin)
		if isPreflight(r) {
			if allowValue == "" {
				// 非放行来源：不下发任何 CORS 头，交给下游统一处理，
				// 浏览器会因缺少 Access-Control-Allow-Origin 拒绝响应。
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", allowValue)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			if requested := strings.TrimSpace(r.Header.Get("Access-Control-Request-Headers")); requested != "" {
				// 回显浏览器声明的请求头，客户端后续加自定义头时无需改服务端。
				w.Header().Set("Access-Control-Allow-Headers", requested)
			} else {
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Admin-Key, X-App-Id, X-Opencode-Session")
			}
			w.Header().Set("Access-Control-Max-Age", corsMaxAgeSeconds)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if allowValue != "" {
			w.Header().Set("Access-Control-Allow-Origin", allowValue)
		}
		if policy != "*" {
			// 白名单模式下响应随 Origin 变化，必须提示缓存别跨源复用。
			w.Header().Add("Vary", "Origin")
		}
		next.ServeHTTP(w, r)
	})
}

// allowOriginValue 返回应写入的 Access-Control-Allow-Origin 值，空串表示不放行。
// policy 为 "*" 时无论是否带 Origin 都返回 "*"；白名单模式要求请求 Origin
// 与某一条目精确匹配（大小写不敏感）。
func allowOriginValue(policy, origin string) string {
	if policy == "*" {
		return "*"
	}
	if origin == "" {
		return ""
	}
	for _, allowed := range strings.Split(policy, ",") {
		if strings.EqualFold(strings.TrimSpace(allowed), origin) {
			return origin
		}
	}
	return ""
}

// isPreflight 识别 CORS 预检请求：OPTIONS 且声明了实际要用的方法。
func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
}
