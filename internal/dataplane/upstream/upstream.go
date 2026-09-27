// Package upstream 编译 endpoint 池，并分离只读拓扑和原子运行状态
package upstream

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/helantianshen/gateway/internal/dataplane/balancer"
	"github.com/helantianshen/gateway/internal/dataplane/proxy"
)

var (
	// ErrNoHealthyEndpoint 表示一个 upstream 当前没有可接收请求的 endpoint
	ErrNoHealthyEndpoint = errors.New("upstream has no healthy endpoint")
)

// ProxyMode 标识 upstream 所需的 Host 转发模式；只为实际使用的模式创建 Proxy
type ProxyMode uint8

const (
	ProxyModeDefault ProxyMode = 1 << iota
	ProxyModePreserveHost

	allProxyModes = ProxyModeDefault | ProxyModePreserveHost
)

// EndpointConfig 是 bootstrap 装配 runtime upstream 时传入的值对象
// Target 使用值类型，NewCompiledUpstream 会再次复制，调用方后续修改不会影响运行时
type EndpointConfig struct {
	ID     string
	Target url.URL
	Weight int
}

// EndpointState 保存 endpoint 的独立可变运行状态
//
// endpoint 初始为健康状态；activeRequest 覆盖完整代理调用及流式传输
type EndpointState struct {
	healthy       atomic.Bool
	activeRequest atomic.Int64
}

func newEndpointState() *EndpointState {
	state := &EndpointState{}
	state.healthy.Store(true)
	return state
}

// Healthy 返回 endpoint 当前是否允许被负载均衡器选择
func (s *EndpointState) Healthy() bool {
	return s != nil && s.healthy.Load()
}

// SetHealthy 原子更新 endpoint 健康状态
func (s *EndpointState) SetHealthy(healthy bool) {
	if s != nil {
		s.healthy.Store(healthy)
	}
}

// ActiveRequests 返回当前正在此 endpoint 上执行的代理请求数
func (s *EndpointState) ActiveRequests() int64 {
	if s == nil {
		return 0
	}
	return s.activeRequest.Load()
}

// CompiledEndpoint 是 Application 生命周期内地址和身份稳定的 endpoint
// target、weight 与 Proxy 创建后不再修改；只有 state 中的原子字段可变
type CompiledEndpoint struct {
	id     string
	target url.URL
	weight int
	state  *EndpointState

	defaultProxy      *proxy.Proxy
	preserveHostProxy *proxy.Proxy
}

// ID 返回配置中稳定的 endpoint ID
func (e *CompiledEndpoint) ID() string {
	if e == nil {
		return ""
	}
	return e.id
}

// Target 返回 endpoint URL 的值拷贝
func (e *CompiledEndpoint) Target() url.URL {
	if e == nil {
		return url.URL{}
	}
	return e.target
}

// Weight 返回配置权重；当前 Round Robin 不读取该值
func (e *CompiledEndpoint) Weight() int {
	if e == nil {
		return 0
	}
	return e.weight
}

// State 返回 endpoint 的稳定运行状态对象
func (e *CompiledEndpoint) State() *EndpointState {
	if e == nil {
		return nil
	}
	return e.state
}

// ServeHTTP 使用创建期固定的 Proxy 转发一个请求
//
// 返回 false 表示当前 endpoint 未编译调用方要求的 preserveHost 模式，且没有写响应
// 返回 true 表示请求已经交给 Proxy，响应由 Proxy 或 upstream 写入
func (e *CompiledEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request, preserveHost bool) bool {
	if e == nil || e.state == nil {
		return false
	}

	selectedProxy := e.defaultProxy
	if preserveHost {
		selectedProxy = e.preserveHostProxy
	}
	if selectedProxy == nil {
		return false
	}

	e.state.activeRequest.Add(1)
	defer e.state.activeRequest.Add(-1)
	selectedProxy.ServeHTTP(w, r)
	return true
}

// CompiledUpstream 是不可变 endpoint pool 与独立 Round Robin 状态的组合
type CompiledUpstream struct {
	id        string
	endpoints []*CompiledEndpoint
	byID      map[string]*CompiledEndpoint
	balancer  balancer.RoundRobin
}

// NewCompiledUpstream 创建一个数据面 upstream pool，并按 modes 预创建固定目标 Proxy
// 所有 Proxy 共享调用方传入的 transport；本函数不会创建或拥有 Transport
func NewCompiledUpstream(
	id string,
	configs []EndpointConfig,
	modes ProxyMode,
	transport http.RoundTripper,
	requestTimeout time.Duration,
) (*CompiledUpstream, error) {
	// 先校验池级参数；modes 为零时仍创建 endpoint 状态，但无需 Transport
	// 有转发模式时每个 endpoint 都必须使用同一个调用方提供的 Transport
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("编译 upstream 失败: id 不能为空")
	}
	if len(configs) == 0 {
		return nil, fmt.Errorf("编译 upstream %q 失败: 至少需要一个 endpoint", id)
	}
	if modes&^allProxyModes != 0 {
		return nil, fmt.Errorf("编译 upstream %q 失败: ProxyMode %d 不合法", id, modes)
	}
	if modes != 0 && transport == nil {
		return nil, fmt.Errorf("编译 upstream %q 失败: transport 不能为空", id)
	}
	if requestTimeout <= 0 {
		return nil, fmt.Errorf("编译 upstream %q 失败: request timeout 必须为正数", id)
	}

	compiled := &CompiledUpstream{
		id:        id,
		endpoints: make([]*CompiledEndpoint, 0, len(configs)),
		byID:      make(map[string]*CompiledEndpoint, len(configs)),
	}
	// 每个 endpoint 拥有独立的健康与活跃请求状态，拓扑和 Proxy 在装配后只读
	// 同时构建顺序切片供 RR 使用、按 ID 索引供管理和指标绑定使用
	for index, config := range configs {
		if strings.TrimSpace(config.ID) == "" {
			return nil, fmt.Errorf("编译 upstream %q 失败: endpoints[%d].id 不能为空", id, index)
		}
		if _, exists := compiled.byID[config.ID]; exists {
			return nil, fmt.Errorf("编译 upstream %q 失败: endpoint id %q 重复", id, config.ID)
		}
		if config.Weight <= 0 {
			return nil, fmt.Errorf("编译 upstream %q 失败: endpoint %q weight 必须为正数", id, config.ID)
		}
		if err := validateTarget(config.Target); err != nil {
			return nil, fmt.Errorf("编译 upstream %q 失败: endpoint %q target 不合法: %w", id, config.ID, err)
		}

		endpoint := &CompiledEndpoint{
			id:     config.ID,
			target: config.Target,
			weight: config.Weight,
			state:  newEndpointState(),
		}
		// Host 模式由引用此 upstream 的路由决定；未使用的模式不创建 Proxy
		// 两种模式指向同一 endpoint，只有 Rewrite 时的 Host 处理不同
		if modes&ProxyModeDefault != 0 {
			target := endpoint.target
			endpoint.defaultProxy = proxy.New(&target, transport, requestTimeout, false)
		}
		if modes&ProxyModePreserveHost != 0 {
			target := endpoint.target
			endpoint.preserveHostProxy = proxy.New(&target, transport, requestTimeout, true)
		}

		compiled.endpoints = append(compiled.endpoints, endpoint)
		compiled.byID[endpoint.id] = endpoint
	}
	return compiled, nil
}

// ID 返回逻辑 upstream ID
func (u *CompiledUpstream) ID() string {
	if u == nil {
		return ""
	}
	return u.id
}

// Endpoint 返回指定 ID 的稳定 endpoint 指针，主要供健康状态管理和观测读取
func (u *CompiledUpstream) Endpoint(id string) (*CompiledEndpoint, bool) {
	if u == nil {
		return nil, false
	}
	endpoint, ok := u.byID[id]
	return endpoint, ok
}

// EndpointCount 返回 pool 中的 endpoint 数量
func (u *CompiledUpstream) EndpointCount() int {
	if u == nil {
		return 0
	}
	return len(u.endpoints)
}

// Select 使用 upstream 私有的 Round Robin 状态选择一个健康 endpoint
func (u *CompiledUpstream) Select() (*CompiledEndpoint, error) {
	if u == nil || len(u.endpoints) == 0 {
		return nil, ErrNoHealthyEndpoint
	}
	index, ok := u.balancer.Select(len(u.endpoints), func(index int) bool {
		return u.endpoints[index].state.Healthy()
	})
	if !ok {
		return nil, ErrNoHealthyEndpoint
	}
	return u.endpoints[index], nil
}

func validateTarget(target url.URL) error {
	if target.User != nil {
		return errors.New("URL 不允许包含 userinfo")
	}
	if !target.IsAbs() || target.Host == "" {
		return errors.New("必须是包含 host 的绝对 URL")
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return errors.New("URL 协议必须为 http 或 https")
	}
	return nil
}
