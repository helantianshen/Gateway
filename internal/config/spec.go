package config

// ConfigSpec 是 gateway YAML 文件的顶层声明式配置
//
// upstream 包含 endpoint，route 通过 ID 引用 upstream
type ConfigSpec struct {
	APIVersion string         `yaml:"api_version"`
	Upstreams  []UpstreamSpec `yaml:"upstreams"`
	Routes     []RouteSpec    `yaml:"routes"`
	Policies   PolicySpec     `yaml:"policies"`
}

// UpstreamSpec 定义一个逻辑上游及其服务端点集合
//
// 配置校验要求至少一个 endpoint
type UpstreamSpec struct {
	ID        string         `yaml:"id"`
	Endpoints []EndpointSpec `yaml:"endpoints"`
}

// EndpointSpec 定义一个可实际建立连接的上游地址
// Weight 必须为正整数；YAML 未显式填写 weight 时，加载器会应用默认值 100
type EndpointSpec struct {
	ID     string `yaml:"id"`
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight"`
}

// RouteSpec 定义一条路由；Upstream 引用逻辑 ID，不直接绑定 endpoint URL
type RouteSpec struct {
	ID           string `yaml:"id"`
	Host         string `yaml:"host"`
	Path         string `yaml:"path"`
	Method       string `yaml:"method"`
	Upstream     string `yaml:"upstream"`
	Priority     int    `yaml:"priority"`
	PreserveHost bool   `yaml:"preserve_host"`
}

// PolicySpec 定义全局策略；Rate 与 Burst 仅校验，尚不参与请求处理
type PolicySpec struct {
	RequestTimeout string `yaml:"request_timeout"`
	Rate           int    `yaml:"rate"`
	Burst          int    `yaml:"burst"`
}
