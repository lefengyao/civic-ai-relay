package store

import (
	"context"
	"strings"
	"testing"
)

// 上游地址校验的回归测试。现场事故（2026-09-16）：操作员填了没有协议头的
// "127.0.0.1:6000"，管理台只回一个 provider_invalid，真实原因被丢弃，
// 只能靠读代码定位。这里把"哪些写法通过、哪些被拒、拒绝时说什么"钉住。
func TestValidateProviderURL(t *testing.T) {
	accepted := []struct{ in, want string }{
		{"http://127.0.0.1:6000", "http://127.0.0.1:6000"},
		{"https://127.0.0.1:6000", "https://127.0.0.1:6000"},
		{"http://localhost:6000", "http://localhost:6000"},
		{"http://foo.localhost:6000", "http://foo.localhost:6000"},
		{"http://model.test:6000", "http://model.test:6000"},
		{"http://[::1]:6000", "http://[::1]:6000"},
		{"http://172.17.0.1:6000", "http://172.17.0.1:6000"},
		{"http://192.168.1.50:6000", "http://192.168.1.50:6000"},
		{"http://10.0.0.5:6000", "http://10.0.0.5:6000"},
		{"https://api.example.com/v1", "https://api.example.com"},
		{"http://localhost:6000/v1/", "http://localhost:6000"},
		{"  https://api.example.com  ", "https://api.example.com"},
	}
	for _, tc := range accepted {
		got, err := validateProviderURL(tc.in)
		if err != nil {
			t.Errorf("validateProviderURL(%q) = error %v, want %q", tc.in, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("validateProviderURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	rejected := []struct{ in, want string }{
		{"", "不能为空"},
		// 缺协议头：Go 的 url.Parse 自己只会报 "first path segment in URL
		// cannot contain colon"，对操作员毫无指向性，必须换成能照抄的说明。
		{"127.0.0.1:6000", "协议头"},
		{"localhost:6000", "协议头"},
		{"api.example.com", "协议头"},
		// 容器/宿主机场景里最容易被误填的两个主机名，都不在 HTTP 放行名单内。
		{"http://host.docker.internal:6000", "放行名单"},
		{"http://model-service:6000", "放行名单"},
		// 公网地址必须 HTTPS。
		{"http://8.8.8.8:6000", "放行名单"},
		{"http://api.example.com", "放行名单"},
		{"http://0.0.0.0:6000", "放行名单"},
		{"https://user:pass@api.example.com", "不带账号"},
		{"https://api.example.com?k=v", "不带账号"},
		{"https://api.example.com#frag", "不带账号"},
		{"https://api.example.com:70000", "端口"},
	}
	for _, tc := range rejected {
		_, err := validateProviderURL(tc.in)
		if err == nil {
			t.Errorf("validateProviderURL(%q) = nil error, want rejection", tc.in)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("validateProviderURL(%q) error = %q, want it to mention %q", tc.in, err.Error(), tc.want)
		}
	}
}

func TestCreateProviderExplainsBadBaseURL(t *testing.T) {
	repo := newTestStore(t)
	_, err := repo.CreateProvider(context.Background(), NewProvider{Name: "local", BaseURL: "127.0.0.1:6000", APIKey: "k"})
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:6000") {
		t.Fatalf("err = %v, want a message quoting the rejected value", err)
	}
}

func TestUpdateProviderExplainsDuplicateName(t *testing.T) {
	repo := newTestStore(t)
	ctx := context.Background()
	if _, err := repo.CreateProvider(ctx, NewProvider{Name: "A", BaseURL: "https://a.example", APIKey: "ka"}); err != nil {
		t.Fatal(err)
	}
	second, err := repo.CreateProvider(ctx, NewProvider{Name: "B", BaseURL: "https://b.example", APIKey: "kb"})
	if err != nil {
		t.Fatal(err)
	}
	// providers.name 是 UNIQUE：改名撞车时必须给出可读说明，而不是 SQLite 原文。
	if _, err := repo.UpdateProvider(ctx, second.ID, UpdateProvider{Name: "A"}); err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("err = %v, want a duplicate-name explanation", err)
	}
}

func TestUpdateProviderExplainsMissingProvider(t *testing.T) {
	repo := newTestStore(t)
	if _, err := repo.UpdateProvider(context.Background(), 9999, UpdateProvider{}); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("err = %v, want a missing-provider explanation", err)
	}
}
