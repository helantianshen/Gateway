package config

// ConfigSpec 是 gateway YAML 文件的顶层声明式配置。
//
// 该模型刻意同时保留 upstream、endpoint 与 route 三个层次：Phase 2 运行时虽然只会
// 编译单条兜底路由和单个 endpoint，但 schema 已能稳定承载 Phase 3 的路由匹配与
// Phase 4 的多 endpoint 负载均衡，后续阶段无需对现有 YAML 做破坏性迁移。
type ConfigSpec struct {
	APIVersion string         `yaml:"api_version"`
	Upstreams  []UpstreamSpec `yaml:"upstreams"`
	Routes     []RouteSpec    `yaml:"routes"`
	Policies   PolicySpec     `yaml:"policies"`
}

// UpstreamSpec 定义一个逻辑上游及其服务端点集合。
//
// Phase 2 只允许被 route 引用的 upstream 含有一个 endpoint；Endpoints 保持切片形态，
// 是为了让 Phase 4 可以直接在同一配置模型上实现负载均衡，而不是重新设计配置格式。
type UpstreamSpec struct {
	ID        string         `yaml:"id"`
	Endpoints []EndpointSpec `yaml:"endpoints"`
}

// EndpointSpec 定义一个可实际建立连接的上游地址。
// Weight 必须为正整数；YAML 未显式填写 weight 时，加载器会应用默认值 100。
type EndpointSpec struct {
	ID     string `yaml:"id"`
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight"`
}

// RouteSpec 定义一条声明式路由规则。
//
// Host、Method、Priority 和 PreserveHost 是为 Phase 3 预留的稳定字段。Phase 2 只接受
// Host 与 Method 为空、Path 为 "/" 的单条 catch-all route，不在本阶段实现路由匹配。
type RouteSpec struct {
	ID           string `yaml:"id"`
	Host         string `yaml:"host"`
	Path         string `yaml:"path"`
	Method       string `yaml:"method"`
	Upstream     string `yaml:"upstream"`
	Priority     int    `yaml:"priority"`
	PreserveHost bool   `yaml:"preserve_host"`
}

// PolicySpec 定义当前配置版本中的全局策略。
// RequestTimeout 在 Phase 2 已参与代理请求总超时；Rate 与 Burst 仅完成严格解析和范围
// 校验，限流策略的实际执行属于后续阶段，本阶段不会静默启用尚未实现的限流行为。
type PolicySpec struct {
	RequestTimeout string `yaml:"request_timeout"`
	Rate           int    `yaml:"rate"`
	Burst          int    `yaml:"burst"`
}
