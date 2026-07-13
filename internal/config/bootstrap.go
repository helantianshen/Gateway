// Package config 负责从环境变量读取网关进程的最小启动配置。
//
// 职责：
//   - 从环境变量读取启动阶段必需的监听地址、upstream 地址、请求超时
//     和 Graceful Shutdown 超时；
//   - 对缺失的配置项使用安全默认值；
//   - 对显式设置但非法的 upstream URL 或非正时长返回错误，阻止进程启动。
//
// 非职责（本轮明确不实现）：
//   - 不引入 YAML/JSON 配置文件解析；
//   - 不与 etcd/Redis 等外部配置中心交互；
//   - 不读取路由、负载均衡、限流等 Phase 2+ 配置。
package config

import (
	"fmt"
	"net/url"
	"os"
	"time"
)

// 默认配置常量。当对应环境变量缺失时使用这些值，
// 确保网关在无显式配置的情况下也能以安全参数启动。
const (
	defaultPublicAddr      = ":8080" // public Server 默认监听所有网卡的 8080 端口
	defaultAdminAddr       = ":9090" // admin Server 默认监听所有网卡的 9090 端口
	defaultUpstreamURL     = "http://127.0.0.1:18080"
	defaultRequestTimeout  = 3 * time.Second  // 单次代理请求的默认总超时
	defaultShutdownTimeout = 10 * time.Second // Graceful Shutdown 默认超时
)

// 环境变量名称常量。集中定义便于测试和后续维护时统一引用。
const (
	envPublicAddr      = "GATEWAY_PUBLIC_ADDR"      // public Server 监听地址
	envAdminAddr       = "GATEWAY_ADMIN_ADDR"       // admin Server 监听地址
	envUpstreamURL     = "GATEWAY_UPSTREAM_URL"     // 反向代理目标 upstream 地址
	envRequestTimeout  = "GATEWAY_REQUEST_TIMEOUT"  // 单次代理请求总超时
	envShutdownTimeout = "GATEWAY_SHUTDOWN_TIMEOUT" // Graceful Shutdown 超时
)

// Config 是网关进程的最小启动配置。
//
// 该结构体在进程启动时一次性读取并校验，运行期间不可变。
// Phase 1 在 Phase 0 基础上增加了 upstream URL 和请求超时配置。
type Config struct {
	// PublicAddr 是 public HTTP Server 的监听地址，格式为 "host:port"。
	// public Server 面向业务流量，挂载反向代理 Handler。
	PublicAddr string

	// AdminAddr 是 admin HTTP Server 的监听地址，格式为 "host:port"。
	// admin Server 面向运维和管理接口，提供 /livez 和 /readyz 健康端点。
	AdminAddr string

	// UpstreamURL 是反向代理的目标 upstream 地址，已解析为 *url.URL。
	// 所有 public 流量将被转发到此地址。
	// 必须是 http 或 https 的绝对 URL，非法值会导致 Load 返回错误。
	UpstreamURL *url.URL

	// RequestTimeout 是单次代理请求的总超时时长。
	// 从请求进入网关到 upstream 响应完成的整个链路受此时长约束。
	// 超时后网关向客户端返回 504 Gateway Timeout。
	// 必须为正值，非正值会导致 Load 返回错误。
	RequestTimeout time.Duration

	// ShutdownTimeout 是 Graceful Shutdown 的最大等待时长。
	//
	// 收到 SIGINT/SIGTERM 后，进程会停止接受新连接并等待活跃连接处理完毕。
	// 如果在此超时内连接未全部关闭，Shutdown 会返回 context 超时错误，
	// 随后进程强制退出。该超时防止进程因个别慢连接而无限期挂起。
	// 必须为正值，非正值会导致 Load 返回错误。
	ShutdownTimeout time.Duration
}

// Load 从环境变量读取配置并校验。
//
// 对于缺失的环境变量，使用安全默认值。
// 对于显式设置但非法的 upstream URL（无法解析或非 http/https scheme），
// 返回错误以阻止进程启动。
// 对于显式设置但解析失败或非正值的时长配置，同样返回错误。
//
// 与 Phase 0 的区别：Phase 0 中所有非法配置静默回退到默认值。
// Phase 1 对 upstream URL 和请求超时引入严格校验，
// 因为这两个值直接影响代理链路的正确性和安全性，
// 使用错误默认值可能导致流量发送到错误地址或请求无限期挂起。
func Load() (*Config, error) {
	cfg := &Config{
		PublicAddr:      getenv(envPublicAddr, defaultPublicAddr),
		AdminAddr:       getenv(envAdminAddr, defaultAdminAddr),
		RequestTimeout:  defaultRequestTimeout,
		ShutdownTimeout: defaultShutdownTimeout,
	}

	// 解析 upstream URL。
	// 缺失时使用默认值；显式设置但非法时返回错误。
	upstreamStr := getenv(envUpstreamURL, defaultUpstreamURL)
	parsed, err := url.Parse(upstreamStr)
	if err != nil {
		return nil, fmt.Errorf("upstream URL 解析失败 %q: %w", upstreamStr, err)
	}
	if !parsed.IsAbs() {
		return nil, fmt.Errorf("upstream URL 必须是绝对地址 %q", upstreamStr)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("upstream URL scheme 必须是 http 或 https %q", upstreamStr)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("upstream URL 必须包含 host %q", upstreamStr)
	}
	cfg.UpstreamURL = parsed

	// 解析请求超时。缺失时使用默认值；显式设置但非法时返回错误。
	if v := os.Getenv(envRequestTimeout); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("请求超时解析失败 %q: %w", v, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("请求超时必须为正值 %q", v)
		}
		cfg.RequestTimeout = d
	}

	// 解析 Shutdown 超时。缺失时使用默认值；显式设置但非法时返回错误。
	if v := os.Getenv(envShutdownTimeout); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("shutdown 超时解析失败 %q: %w", v, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("shutdown 超时必须为正值 %q", v)
		}
		cfg.ShutdownTimeout = d
	}

	return cfg, nil
}

// getenv 读取环境变量，如果为空则返回默认值。
func getenv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
