// Package config 将本地启动参数和 YAML 业务配置编译为静态运行时配置
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

	// 旧变量只用于拒绝遗留部署配置
	envUpstreamURL    = "GATEWAY_UPSTREAM_URL"
	envRequestTimeout = "GATEWAY_REQUEST_TIMEOUT"
)

// BootstrapConfig 是随部署环境变化的本地启动参数
// ConfigFile 也作为语义校验错误中的文件路径使用；业务配置本身不复制到环境变量
// 从而保持 YAML 是 upstream、route 和 policy 的唯一事实来源
type BootstrapConfig struct {
	ConfigFile      string
	PublicAddr      string
	AdminAddr       string
	ShutdownTimeout time.Duration
}

// EndpointTarget 是配置层编译后的不可变 endpoint 定义
// URL 使用值类型，避免运行时保留可被调用方替换的 *url.URL 指针
type EndpointTarget struct {
	ID     string
	URL    url.URL
	Weight int
}

// UpstreamTarget 是逻辑 upstream 的强类型 endpoint 集合
type UpstreamTarget struct {
	ID        string
	Endpoints []EndpointTarget
}

// Config 是经过完整校验、可直接交给 Application 的强类型运行时配置
// Application 从 Upstreams 构建 endpoint pool，从 Spec.Routes 构建 Router
type Config struct {
	PublicAddr      string
	AdminAddr       string
	Upstreams       map[string]UpstreamTarget
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	Spec            *ConfigSpec
}

// Load 执行生产启动所需的完整配置管线：读取环境变量、严格加载 YAML、校验并编译
// 任一步失败都不会产生部分可用的 Config，调用方必须在创建任何 listener 前处理错误
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

// LoadBootstrapConfig 使用 os.LookupEnv 读取本地启动参数
//
// LookupEnv 能严格区分“未设置”和“已设置为空字符串”：前者采用默认值，后者作为明确的错误输入被拒绝
// 旧业务环境变量即使显式设置为空也会触发迁移错误，避免旧部署清单看似启动成功、实际却已经不再生效
func LoadBootstrapConfig() (BootstrapConfig, error) {
	// 从固定默认值开始叠加环境变量；显式空值记录为错误，不回退默认值
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

	// 旧业务变量即使为空也必须报错，避免部署以为它仍影响运行时
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

// Compile 先校验 ConfigSpec，再生成带强类型 URL 和超时的运行配置
// 结果保留 Spec 指针；调用方在 Application 使用期间不得修改该配置
func Compile(spec *ConfigSpec, bootstrap BootstrapConfig) (*Config, error) {
	// 先校验 YAML 的结构、引用与路由语义，再检查本地监听和停机参数
	// 任一步失败都不会返回可供 Application 使用的部分配置
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

	// 每个 endpoint 的 URL 解析为值，运行时目标不共享 YAML 中的字符串字段
	// URL 已经通过 Validate；解析失败仍向调用方返回明确错误
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

	// 请求超时在数据面调用前转换一次，避免请求路径重复解析字符串
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
