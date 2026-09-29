// Package router 编译不可变路由树，并按 Host、Method、Path 匹配请求
package router

// MatchResult 是路由匹配成功时返回的结果
//
// RouteID 和 UpstreamID 是配置中声明的稳定标识符
// PathTemplate 是编译期路径模式，只能用于日志和低基数指标，不能回退为原始 URL
// Params 是从路径参数段（:param）和 catch-all 段（*path）提取的解码后键值对
// PreserveHost 指示代理是否应保留客户端原始 Host 头
type MatchResult struct {
	RouteID      string
	PathTemplate string
	UpstreamID   string
	Params       map[string]string
	PreserveHost bool
}

// MatchErrorCode 表示路由匹配失败的类型
type MatchErrorCode int

const (
	// MatchErrIllegalPath 表示请求路径包含非法编码、分隔符、控制字符或 dot segment
	// 对应 HTTP 400 Bad Request
	MatchErrIllegalPath MatchErrorCode = iota

	// MatchErrNoRoute 表示没有路由匹配该请求
	// 对应 HTTP 404 Not Found
	MatchErrNoRoute

	// MatchErrIllegalHost 表示非空请求 Host 不满足 DNS/IP authority 约束，对应 HTTP 400
	MatchErrIllegalHost

	// MatchErrMethodNotAllowed 表示 Host/Path 存在但 Method 不允许，对应 HTTP 405
	MatchErrMethodNotAllowed
)

// MatchError 是路由匹配失败时返回的错误
//
// GatewayHandler 根据 Code 将其转换为对应的 HTTP 状态码
// Message 包含人类可读的错误原因，但不应包含敏感信息
type MatchError struct {
	Code    MatchErrorCode
	Message string
	// AllowedMethods 仅在 MethodNotAllowed 时包含去重并按字典序排列的方法
	AllowedMethods []string
}

// Error 返回按错误类别标注的匹配失败原因
func (e *MatchError) Error() string {
	switch e.Code {
	case MatchErrIllegalPath:
		return "非法请求路径: " + e.Message
	case MatchErrMethodNotAllowed:
		return "请求方法不允许: " + e.Message
	case MatchErrIllegalHost:
		return "非法请求 Host: " + e.Message
	case MatchErrNoRoute:
		return "无匹配路由: " + e.Message
	default:
		return e.Message
	}
}

// segmentType 表示路径段的匹配模式
type segmentType int

const (
	segStatic   segmentType = iota // 静态段，如 /users
	segParam                       // 参数段，如 /:id
	segCatchAll                    // catch-all 段，如 /*path
)

// compiledRoute 是编译后的单条路由，存储在 Radix Tree 的叶子节点中
// 所有字段在编译后不可变
type compiledRoute struct {
	routeID      string
	pathTemplate string
	upstreamID   string
	preserveHost bool
	priority     int
	hostPattern  hostPattern
	method       string
	// paramNames 按路径中出现顺序记录参数名，用于匹配后构建 Params
	// catch-all 的参数名也记录在此处
	paramNames []string
}

// hostPattern 表示路由的 Host 匹配模式
type hostPattern struct {
	kind hostKind
	// value 对 exact 类型是归一化后的 hostname
	// 对 wildcard 类型是去除了 *." 后的域名后缀
	// 对 any 类型为空
	value string
}

type hostKind int

const (
	hostAny      hostKind = iota // 空 host，匹配所有
	hostExact                    // 精确 host，如 api.example.com
	hostWildcard                 // 通配 host，如 *.example.com
)
