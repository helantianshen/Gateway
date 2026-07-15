// Package config 负责把本地启动参数与声明式 YAML 配置编译为网关运行时配置。
//
// 配置来源边界：
//   - 环境变量只负责配置文件路径、public/admin 监听地址和优雅停机超时；
//   - YAML 负责 upstream、route 与 policy 等业务配置；
//   - Phase 0/1 的 upstream URL 和请求超时环境变量已移除，若仍设置会返回迁移错误。
//
// 本包完成静态加载、严格校验和强类型 upstream 编译，并调用生产 Router 编译器校验
// 路由集合；请求侧匹配与负载均衡由数据面执行。本包不持有 endpoint 运行状态，
// 也不实现动态配置或配置中心。
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultConfigFile      = "configs/gateway.yaml"
	defaultPublicAddr      = ":8080"
	defaultAdminAddr       = ":9090"
	defaultShutdownTimeout = 10 * time.Second
)

const (
	envConfigFile      = "GATEWAY_CONFIG_FILE"
	envPublicAddr      = "GATEWAY_PUBLIC_ADDR"
	envAdminAddr       = "GATEWAY_ADMIN_ADDR"
	envShutdownTimeout = "GATEWAY_SHUTDOWN_TIMEOUT"

	// 这两个旧变量只用于迁移检测，绝不能再参与运行配置编译。
	envUpstreamURL    = "GATEWAY_UPSTREAM_URL"
	envRequestTimeout = "GATEWAY_REQUEST_TIMEOUT"
)

// BootstrapConfig 是随部署环境变化的本地启动参数。
// ConfigFile 也作为语义校验错误中的文件路径使用；业务配置本身不复制到环境变量，
// 从而保持 YAML 是 upstream、route 和 policy 的唯一事实来源。
type BootstrapConfig struct {
	ConfigFile      string
	PublicAddr      string
	AdminAddr       string
	ShutdownTimeout time.Duration
}

// EndpointTarget 是配置层编译后的不可变 endpoint 定义。
// URL 使用值类型，避免运行时保留可被调用方替换的 *url.URL 指针。
type EndpointTarget struct {
	ID     string
	URL    url.URL
	Weight int
}

// UpstreamTarget 是逻辑 upstream 的强类型 endpoint 集合。
type UpstreamTarget struct {
	ID        string
	Endpoints []EndpointTarget
}

// Config 是经过完整校验、可直接交给 Application 的强类型运行时配置。
// Application 从 Upstreams 构建 endpoint pool，从 Spec.Routes 构建 Router。
type Config struct {
	PublicAddr      string
	AdminAddr       string
	Upstreams       map[string]UpstreamTarget
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	Spec            *ConfigSpec
}

// Load 执行生产启动所需的完整配置管线：读取环境变量、严格加载 YAML、校验并编译。
// 任一步失败都不会产生部分可用的 Config，调用方必须在创建任何 listener 前处理错误。
func Load() (*Config, error) {
	bootstrap, err := LoadBootstrapConfig()
	if err != nil {
		return nil, err
	}
	spec, err := LoadConfig(bootstrap.ConfigFile)
	if err != nil {
		return nil, err
	}
	return Compile(spec, bootstrap)
}

// LoadBootstrapConfig 使用 os.LookupEnv 读取本地启动参数。
//
// LookupEnv 能严格区分“未设置”和“已设置为空字符串”：前者采用默认值，后者作为
// 明确的错误输入被拒绝。旧业务环境变量即使显式设置为空也会触发迁移错误，避免旧
// 部署清单看似启动成功、实际却已经不再生效。
func LoadBootstrapConfig() (BootstrapConfig, error) {
	bootstrap := BootstrapConfig{
		ConfigFile:      defaultConfigFile,
		PublicAddr:      defaultPublicAddr,
		AdminAddr:       defaultAdminAddr,
		ShutdownTimeout: defaultShutdownTimeout,
	}
	var problems []string

	readStringEnv(envConfigFile, &bootstrap.ConfigFile, &problems)
	readStringEnv(envPublicAddr, &bootstrap.PublicAddr, &problems)
	readStringEnv(envAdminAddr, &bootstrap.AdminAddr, &problems)

	if raw, exists := os.LookupEnv(envShutdownTimeout); exists {
		if raw == "" {
			problems = append(problems, envShutdownTimeout+" 已设置但为空")
		} else if duration, err := time.ParseDuration(raw); err != nil {
			problems = append(problems, envShutdownTimeout+" 必须是合法的 Go duration")
		} else if duration <= 0 {
			problems = append(problems, envShutdownTimeout+" 必须为正数 duration")
		} else {
			bootstrap.ShutdownTimeout = duration
		}
	}

	if _, exists := os.LookupEnv(envUpstreamURL); exists {
		problems = append(problems, envUpstreamURL+" 已移除：upstream URL 现在必须在 YAML 的 upstreams[].endpoints[].url 中配置")
	}
	if _, exists := os.LookupEnv(envRequestTimeout); exists {
		problems = append(problems, envRequestTimeout+" 已移除：request timeout 现在必须在 YAML 的 policies.request_timeout 中配置")
	}

	if len(problems) > 0 {
		return BootstrapConfig{}, fmt.Errorf("启动环境配置校验失败:\n  %s", strings.Join(problems, "\n  "))
	}
	return bootstrap, nil
}

func readStringEnv(key string, target *string, problems *[]string) {
	if value, exists := os.LookupEnv(key); exists {
		if value == "" {
			*problems = append(*problems, key+" 已设置但为空")
			return
		}
		*target = value
	}
}

// Compile 把完整 ConfigSpec 与本地 BootstrapConfig 编译成 Application 使用的 Config。
// Validate 由 Compile 自身调用，确保直接使用该公开入口的调用方也无法绕过结构、语义
// 或 Phase 4 运行约束。Validate 成功后再做 URL 和 duration 的强类型转换；转换失败
// 理论上不可达，仍保留防御性错误以防未来校验规则与编译逻辑发生偏移。
//
// Phase 4：从所有 upstream 提取全部 endpoint，生成强类型 Upstreams map。
// 路由语法和冲突检测由 Validate 与 Application 共同调用 router.Compile 完成。
func Compile(spec *ConfigSpec, bootstrap BootstrapConfig) (*Config, error) {
	file := bootstrap.ConfigFile
	if err := Validate(spec, file); err != nil {
		return nil, err
	}
	if bootstrap.PublicAddr == "" {
		return nil, fmt.Errorf("编译配置失败: public 监听地址不能为空")
	}
	if bootstrap.AdminAddr == "" {
		return nil, fmt.Errorf("编译配置失败: admin 监听地址不能为空")
	}
	if bootstrap.ShutdownTimeout <= 0 {
		return nil, fmt.Errorf("编译配置失败: shutdown 超时必须为正数")
	}

	// 为每个 upstream 深拷贝全部 endpoint 定义。URL 在 Validate 后理论上都可解析；
	// 仍保留防御性错误，避免未来校验与编译规则漂移。
	upstreams := make(map[string]UpstreamTarget, len(spec.Upstreams))
	for _, upstream := range spec.Upstreams {
		if len(upstream.Endpoints) == 0 {
			return nil, fmt.Errorf("编译配置失败: upstream %q 至少需要一个 endpoint", upstream.ID)
		}
		target := UpstreamTarget{
			ID:        upstream.ID,
			Endpoints: make([]EndpointTarget, 0, len(upstream.Endpoints)),
		}
		for _, endpoint := range upstream.Endpoints {
			parsed, err := url.Parse(endpoint.URL)
			if err != nil {
				return nil, fmt.Errorf("编译配置失败: upstream %q 的 endpoint %q URL 无法解析: %w", upstream.ID, endpoint.ID, err)
			}
			target.Endpoints = append(target.Endpoints, EndpointTarget{
				ID:     endpoint.ID,
				URL:    *parsed,
				Weight: endpoint.Weight,
			})
		}
		upstreams[upstream.ID] = target
	}

	requestTimeout, err := time.ParseDuration(spec.Policies.RequestTimeout)
	if err != nil {
		return nil, fmt.Errorf("编译配置失败: 已校验的 request_timeout 无法解析")
	}

	return &Config{
		PublicAddr:      bootstrap.PublicAddr,
		AdminAddr:       bootstrap.AdminAddr,
		Upstreams:       upstreams,
		RequestTimeout:  requestTimeout,
		ShutdownTimeout: bootstrap.ShutdownTimeout,
		Spec:            spec,
	}, nil
}
