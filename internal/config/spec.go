package config

// ConfigSpec 是 gateway YAML 文件的顶层声明式配置。
//
// 该模型同时保留 upstream、endpoint 与 route 三个层次：Phase 3 已支持多路由和
// 多 upstream，但每个 upstream 仍只允许一个 endpoint；Phase 4 将在保持 YAML
// 结构兼容的前提下解除多 endpoint 限制。
type ConfigSpec struct {
	APIVersion string         `yaml:"api_version"`
	Upstreams  []UpstreamSpec `yaml:"upstreams"`
	Routes     []RouteSpec    `yaml:"routes"`
	Policies   PolicySpec     `yaml:"policies"`
}

// UpstreamSpec 定义一个逻辑上游及其服务端点集合。
//
// Phase 3 要求每个 upstream 恰好一个 endpoint；Endpoints 保持切片形态，是为了让
// Phase 4 可以直接在同一配置模型上实现负载均衡，而不是重新设计配置格式。
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
// Host、Method、Path、Priority 和 PreserveHost 均已在 Phase 3 参与 Router 编译与
// 数据面匹配；Upstream 保存逻辑 upstream ID，不直接耦合 endpoint URL。
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
// RequestTimeout 参与代理请求总超时；Rate 与 Burst 在 Phase 3 仍只完成严格解析和
// 范围校验，限流策略的实际执行属于后续阶段，不会静默启用尚未实现的行为。
type PolicySpec struct {
	RequestTimeout string `yaml:"request_timeout"`
	Rate           int    `yaml:"rate"`
	Burst          int    `yaml:"burst"`
}
