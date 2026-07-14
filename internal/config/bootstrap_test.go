package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unsetEnv 为依赖 LookupEnv 语义的测试真正移除变量，而不是把变量设置为空字符串。
// 测试结束时恢复调用进程原值，避免本机或 CI 环境污染后续用例。
func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		value, existed := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("移除环境变量 %s 失败: %v", key, err)
		}
		key, value, existed := key, value, existed
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(key, value)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
}

func unsetAllConfigEnv(t *testing.T) {
	t.Helper()
	unsetEnv(t,
		envConfigFile,
		envPublicAddr,
		envAdminAddr,
		envShutdownTimeout,
		envUpstreamURL,
		envRequestTimeout,
	)
}

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}
	return path
}

const validConfigYAML = `api_version: v1
upstreams:
  - id: mock-service
    endpoints:
      - id: mock-1
        url: http://127.0.0.1:18080
        weight: 100
routes:
  - id: default
    path: /
    upstream: mock-service
policies:
  request_timeout: 3s
  rate: 0
  burst: 0
`

func TestLoadBootstrapConfig_DefaultsRequireUnsetVariables(t *testing.T) {
	unsetAllConfigEnv(t)

	bootstrap, err := LoadBootstrapConfig()
	if err != nil {
		t.Fatalf("LoadBootstrapConfig 返回意外错误: %v", err)
	}
	if bootstrap.ConfigFile != defaultConfigFile {
		t.Errorf("ConfigFile = %q, want %q", bootstrap.ConfigFile, defaultConfigFile)
	}
	if bootstrap.PublicAddr != defaultPublicAddr {
		t.Errorf("PublicAddr = %q, want %q", bootstrap.PublicAddr, defaultPublicAddr)
	}
	if bootstrap.AdminAddr != defaultAdminAddr {
		t.Errorf("AdminAddr = %q, want %q", bootstrap.AdminAddr, defaultAdminAddr)
	}
	if bootstrap.ShutdownTimeout != defaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v, want %v", bootstrap.ShutdownTimeout, defaultShutdownTimeout)
	}
}

func TestLoadBootstrapConfig_CustomValues(t *testing.T) {
	unsetAllConfigEnv(t)
	t.Setenv(envConfigFile, "/etc/gateway/config.yaml")
	t.Setenv(envPublicAddr, ":10080")
	t.Setenv(envAdminAddr, ":10090")
	t.Setenv(envShutdownTimeout, "30s")

	bootstrap, err := LoadBootstrapConfig()
	if err != nil {
		t.Fatalf("LoadBootstrapConfig 返回意外错误: %v", err)
	}
	if bootstrap.ConfigFile != "/etc/gateway/config.yaml" || bootstrap.PublicAddr != ":10080" || bootstrap.AdminAddr != ":10090" {
		t.Errorf("字符串启动参数未正确读取: %+v", bootstrap)
	}
	if bootstrap.ShutdownTimeout != 30*time.Second {
		t.Errorf("ShutdownTimeout = %v, want %v", bootstrap.ShutdownTimeout, 30*time.Second)
	}
}

func TestLoadBootstrapConfig_ExplicitEmptyIsRejected(t *testing.T) {
	for _, key := range []string{envConfigFile, envPublicAddr, envAdminAddr, envShutdownTimeout} {
		t.Run(key, func(t *testing.T) {
			unsetAllConfigEnv(t)
			t.Setenv(key, "")
			_, err := LoadBootstrapConfig()
			if err == nil || !strings.Contains(err.Error(), key+" 已设置但为空") {
				t.Fatalf("显式空值错误 = %v, want 包含环境变量名和空值说明", err)
			}
		})
	}
}

func TestLoadBootstrapConfig_InvalidShutdownTimeout(t *testing.T) {
	for _, value := range []string{"invalid", "0s", "-1s"} {
		t.Run(value, func(t *testing.T) {
			unsetAllConfigEnv(t)
			t.Setenv(envShutdownTimeout, value)
			if _, err := LoadBootstrapConfig(); err == nil {
				t.Fatalf("shutdown timeout %q 应被拒绝", value)
			}
		})
	}
}

func TestLoadBootstrapConfig_LegacyVariablesReturnMigrationErrors(t *testing.T) {
	unsetAllConfigEnv(t)
	// 即使值为空，LookupEnv 仍必须识别出旧部署清单还在声明已移除变量。
	t.Setenv(envUpstreamURL, "")
	t.Setenv(envRequestTimeout, "3s")

	_, err := LoadBootstrapConfig()
	if err == nil {
		t.Fatal("旧环境变量未返回迁移错误")
	}
	for _, want := range []string{envUpstreamURL, "YAML 的 upstreams", envRequestTimeout, "policies.request_timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("迁移错误 %q 不包含 %q", err, want)
		}
	}
}

func TestLoad_CompilesYAMLAndEnvironment(t *testing.T) {
	unsetAllConfigEnv(t)
	path := writeConfigFile(t, validConfigYAML)
	t.Setenv(envConfigFile, path)
	t.Setenv(envPublicAddr, "127.0.0.1:10080")
	t.Setenv(envAdminAddr, "127.0.0.1:10090")
	t.Setenv(envShutdownTimeout, "15s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 返回意外错误: %v", err)
	}
	if cfg.PublicAddr != "127.0.0.1:10080" || cfg.AdminAddr != "127.0.0.1:10090" {
		t.Errorf("监听地址编译错误: public=%q admin=%q", cfg.PublicAddr, cfg.AdminAddr)
	}
	if cfg.UpstreamURL.String() != "http://127.0.0.1:18080" {
		t.Errorf("UpstreamURL = %q", cfg.UpstreamURL)
	}
	if cfg.RequestTimeout != 3*time.Second || cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("超时编译错误: request=%v shutdown=%v", cfg.RequestTimeout, cfg.ShutdownTimeout)
	}
	if cfg.Spec == nil || cfg.Spec.Routes[0].ID != "default" {
		t.Fatalf("Config 未保留完整且已校验的 ConfigSpec: %+v", cfg.Spec)
	}
}
