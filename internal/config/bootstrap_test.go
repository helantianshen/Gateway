package config

import (
	"net/url"
	"testing"
	"time"
)

// TestLoad_Defaults 验证当所有环境变量缺失时，Load 返回安全默认值且无错误。
//
// 该测试确保网关在无任何显式配置的情况下也能以有效参数启动，
// 这是最小启动配置的核心保证。
func TestLoad_Defaults(t *testing.T) {
	// 将环境变量设为空字符串，模拟缺失场景。
	t.Setenv(envPublicAddr, "")
	t.Setenv(envAdminAddr, "")
	t.Setenv(envUpstreamURL, "")
	t.Setenv(envRequestTimeout, "")
	t.Setenv(envShutdownTimeout, "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 返回意外错误: %v", err)
	}

	if cfg.PublicAddr != defaultPublicAddr {
		t.Errorf("PublicAddr = %q, want %q", cfg.PublicAddr, defaultPublicAddr)
	}
	if cfg.AdminAddr != defaultAdminAddr {
		t.Errorf("AdminAddr = %q, want %q", cfg.AdminAddr, defaultAdminAddr)
	}
	if cfg.UpstreamURL.String() != defaultUpstreamURL {
		t.Errorf("UpstreamURL = %q, want %q", cfg.UpstreamURL.String(), defaultUpstreamURL)
	}
	if cfg.RequestTimeout != defaultRequestTimeout {
		t.Errorf("RequestTimeout = %v, want %v", cfg.RequestTimeout, defaultRequestTimeout)
	}
	if cfg.ShutdownTimeout != defaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v, want %v", cfg.ShutdownTimeout, defaultShutdownTimeout)
	}
}

// TestLoad_CustomValues 验证 Load 能正确读取环境变量中的自定义配置值。
func TestLoad_CustomValues(t *testing.T) {
	t.Setenv(envPublicAddr, ":10080")
	t.Setenv(envAdminAddr, ":10090")
	t.Setenv(envUpstreamURL, "http://upstream.example.com:8080")
	t.Setenv(envRequestTimeout, "5s")
	t.Setenv(envShutdownTimeout, "30s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 返回意外错误: %v", err)
	}

	if cfg.PublicAddr != ":10080" {
		t.Errorf("PublicAddr = %q, want %q", cfg.PublicAddr, ":10080")
	}
	if cfg.AdminAddr != ":10090" {
		t.Errorf("AdminAddr = %q, want %q", cfg.AdminAddr, ":10090")
	}
	expected, _ := url.Parse("http://upstream.example.com:8080")
	if cfg.UpstreamURL.String() != expected.String() {
		t.Errorf("UpstreamURL = %q, want %q", cfg.UpstreamURL.String(), expected.String())
	}
	if cfg.RequestTimeout != 5*time.Second {
		t.Errorf("RequestTimeout = %v, want %v", cfg.RequestTimeout, 5*time.Second)
	}
	if cfg.ShutdownTimeout != 30*time.Second {
		t.Errorf("ShutdownTimeout = %v, want %v", cfg.ShutdownTimeout, 30*time.Second)
	}
}

// TestLoad_InvalidUpstreamURL 验证当 upstream URL 环境变量为非法值时，
// Load 返回错误以阻止进程启动。
//
// Phase 1 要求非法 URL 必须阻止启动，而不是静默回退到默认值，
// 因为错误的 upstream 会导致所有流量发送到错误地址。
func TestLoad_InvalidUpstreamURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"非绝对路径", "/local/path"},
		{"非 http scheme", "ftp://example.com"},
		{"无 host", "http://"},
		{"无 scheme", "example.com:8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envUpstreamURL, tt.url)
			_, err := Load()
			if err == nil {
				t.Errorf("upstream URL %q 应导致错误，但返回了 nil", tt.url)
			}
		})
	}
}

// TestLoad_InvalidRequestTimeout 验证当请求超时环境变量为非法或非正值时，
// Load 返回错误以阻止进程启动。
func TestLoad_InvalidRequestTimeout(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"非时间字符串", "not-a-duration"},
		{"零值", "0s"},
		{"负值", "-5s"},
		{"负毫秒", "-100ms"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envRequestTimeout, tt.value)
			_, err := Load()
			if err == nil {
				t.Errorf("请求超时 %q 应导致错误，但返回了 nil", tt.value)
			}
		})
	}
}

// TestLoad_InvalidShutdownTimeout 验证当 shutdown 超时为非法或非正值时，
// Load 返回错误。
func TestLoad_InvalidShutdownTimeout(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"非时间字符串", "invalid"},
		{"零值", "0s"},
		{"负值", "-1s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envShutdownTimeout, tt.value)
			_, err := Load()
			if err == nil {
				t.Errorf("shutdown 超时 %q 应导致错误，但返回了 nil", tt.value)
			}
		})
	}
}

// TestLoad_PartialCustom 验证只设置部分环境变量时，
// 未设置的项使用默认值，已设置的项使用自定义值。
func TestLoad_PartialCustom(t *testing.T) {
	t.Setenv(envPublicAddr, ":20080")
	t.Setenv(envAdminAddr, "")
	t.Setenv(envUpstreamURL, "")
	t.Setenv(envRequestTimeout, "")
	t.Setenv(envShutdownTimeout, "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 返回意外错误: %v", err)
	}

	if cfg.PublicAddr != ":20080" {
		t.Errorf("PublicAddr = %q, want %q", cfg.PublicAddr, ":20080")
	}
	if cfg.AdminAddr != defaultAdminAddr {
		t.Errorf("AdminAddr = %q, want %q (默认值)", cfg.AdminAddr, defaultAdminAddr)
	}
	if cfg.UpstreamURL.String() != defaultUpstreamURL {
		t.Errorf("UpstreamURL = %q, want %q (默认值)", cfg.UpstreamURL.String(), defaultUpstreamURL)
	}
	if cfg.RequestTimeout != defaultRequestTimeout {
		t.Errorf("RequestTimeout = %v, want %v (默认值)", cfg.RequestTimeout, defaultRequestTimeout)
	}
	if cfg.ShutdownTimeout != defaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v, want %v (默认值)", cfg.ShutdownTimeout, defaultShutdownTimeout)
	}
}
