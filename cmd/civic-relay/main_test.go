package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestNewServerUsesProvidedAddress(t *testing.T) {
	server := newServer("127.0.0.1:8000", nil)
	if server.Addr != "127.0.0.1:8000" {
		t.Fatalf("server address = %q, want %q", server.Addr, "127.0.0.1:8000")
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.env")
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	content := "ADMIN_API_KEY=adm_test\nRELAY_ENCRYPTION_KEY=" + key + "\n" + body
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestHealthcheckTargetFollowsRelayEnv 确认探针端口跟随 relay.env——端口改了
// 而探针还盯着 8000 会让容器永久处于 unhealthy。
func TestHealthcheckTargetFollowsRelayEnv(t *testing.T) {
	path := writeConfig(t, "HOST=0.0.0.0\nPORT=9123\n")
	t.Setenv("CIVIC_RELAY_CONFIG_FILE", path)
	t.Setenv("PORT", "1234") // 服务端不读环境变量，环境变量不得覆盖文件

	host, port := healthcheckTarget()
	if port != "9123" {
		t.Fatalf("port = %q, want 9123 from relay.env", port)
	}
	if host != "127.0.0.1" {
		t.Fatalf("host = %q, want 127.0.0.1 for a wildcard listen address", host)
	}
}

func TestHealthcheckTargetKeepsExplicitListenAddress(t *testing.T) {
	path := writeConfig(t, "HOST=10.0.0.5\nPORT=8001\n")
	t.Setenv("CIVIC_RELAY_CONFIG_FILE", path)

	host, port := healthcheckTarget()
	if host != "10.0.0.5" || port != "8001" {
		t.Fatalf("target = %s:%s, want 10.0.0.5:8001", host, port)
	}
}

func TestHealthcheckTargetFallsBackWhenConfigMissing(t *testing.T) {
	t.Setenv("CIVIC_RELAY_CONFIG_FILE", filepath.Join(t.TempDir(), "absent.env"))
	t.Setenv("PORT", "9999")

	host, port := healthcheckTarget()
	if host != "127.0.0.1" || port != "9999" {
		t.Fatalf("target = %s:%s, want 127.0.0.1:9999", host, port)
	}
	// 探针必须只读：配置缺失时不能顺手把它创建出来。
	if _, err := os.Stat(filepath.Join(filepath.Dir(os.Getenv("CIVIC_RELAY_CONFIG_FILE")), "relay.env")); err == nil {
		t.Fatal("healthcheck must not create the configuration file")
	}
}

func TestHealthcheckFailsWhenNothingListens(t *testing.T) {
	// 取一个必然无人监听的端口，确保探针在有界时间内失败而不是永久挂起。
	path := writeConfig(t, "HOST=127.0.0.1\nPORT=1\n")
	t.Setenv("CIVIC_RELAY_CONFIG_FILE", path)
	if code := runHealthcheck(); code != 1 {
		t.Fatalf("exit code = %d, want 1 when the server is not listening", code)
	}
}
