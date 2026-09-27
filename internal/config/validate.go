package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/helantianshen/gateway/internal/router"
)

// validationProblems 聚合结构和语义错误，只使用可确定的文件及字段路径
// YAML 行列信息仅由 loader 对语法和 schema 错误提供
type validationProblems struct {
	file     string
	problems []string
}

func (p *validationProblems) add(field, message string) {
	location := p.file
	if location == "" {
		location = "<配置>"
	}
	if field != "" {
		location += ": " + field
	}
	p.problems = append(p.problems, location+": "+message)
}

func (p *validationProblems) err() error {
	if len(p.problems) == 0 {
		return nil
	}
	return fmt.Errorf("配置校验失败:\n  %s", strings.Join(p.problems, "\n  "))
}

// Validate 对 ConfigSpec 执行结构校验、跨字段语义校验和当前运行约束校验
//
// URL 错误有意不包含用户填写的完整原始值，因为 URL 可能意外携带凭据或 token
// route/upstream ID 则属于配置中的非敏感标识符，可以在引用错误中安全回显，以帮助
// 操作者定位拼写问题
func Validate(spec *ConfigSpec, file string) error {
	// 尽量收集独立字段错误，让一次启动给出多个可定位的问题
	// 路由编译仍只返回它遇到的首个语法或冲突错误
	problems := &validationProblems{file: file}
	if spec == nil {
		problems.add("", "配置对象不能为空")
		return problems.err()
	}

	if spec.APIVersion != "v1" {
		problems.add("api_version", "必须为 \"v1\"")
	}

	if len(spec.Upstreams) == 0 {
		problems.add("upstreams", "至少需要配置一个 upstream")
	}
	// 先建立 upstream ID 索引，再遍历路由时检查引用是否存在
	// endpoint ID 的唯一性只在所属 upstream 内校验
	upstreamIndexes := make(map[string]int, len(spec.Upstreams))
	for upstreamIndex, upstream := range spec.Upstreams {
		upstreamPath := fmt.Sprintf("upstreams[%d]", upstreamIndex)
		if strings.TrimSpace(upstream.ID) == "" {
			problems.add(upstreamPath+".id", "不能为空")
		} else if previous, exists := upstreamIndexes[upstream.ID]; exists {
			problems.add(upstreamPath+".id", fmt.Sprintf("与 upstreams[%d].id 重复", previous))
		} else {
			upstreamIndexes[upstream.ID] = upstreamIndex
		}

		if len(upstream.Endpoints) == 0 {
			problems.add(upstreamPath+".endpoints", "至少需要配置一个 endpoint")
		}
		endpointIDs := make(map[string]int, len(upstream.Endpoints))
		for endpointIndex, endpoint := range upstream.Endpoints {
			endpointPath := fmt.Sprintf("%s.endpoints[%d]", upstreamPath, endpointIndex)
			if strings.TrimSpace(endpoint.ID) == "" {
				problems.add(endpointPath+".id", "不能为空")
			} else if previous, exists := endpointIDs[endpoint.ID]; exists {
				problems.add(endpointPath+".id", fmt.Sprintf("与 %s.endpoints[%d].id 重复", upstreamPath, previous))
			} else {
				endpointIDs[endpoint.ID] = endpointIndex
			}

			if strings.TrimSpace(endpoint.URL) == "" {
				problems.add(endpointPath+".url", "不能为空")
			} else {
				validateEndpointURL(problems, endpointPath+".url", endpoint.URL)
			}
			if endpoint.Weight <= 0 {
				problems.add(endpointPath+".weight", "必须为正整数")
			}
		}
	}

	if len(spec.Routes) == 0 {
		problems.add("routes", "至少需要配置一条 route")
	}
	// 路由的基础字段与逻辑 upstream 引用先在配置层检查
	// Host、Method、Path 的语法和路由冲突由下面的 Router 编译器检查
	routeIDs := make(map[string]int, len(spec.Routes))
	for routeIndex, route := range spec.Routes {
		routePath := fmt.Sprintf("routes[%d]", routeIndex)
		if strings.TrimSpace(route.ID) == "" {
			problems.add(routePath+".id", "不能为空")
		} else if previous, exists := routeIDs[route.ID]; exists {
			problems.add(routePath+".id", fmt.Sprintf("与 routes[%d].id 重复", previous))
		} else {
			routeIDs[route.ID] = routeIndex
		}
		if strings.TrimSpace(route.Path) == "" {
			problems.add(routePath+".path", "不能为空")
		}
		if strings.TrimSpace(route.Upstream) == "" {
			problems.add(routePath+".upstream", "不能为空")
		} else if _, exists := upstreamIndexes[route.Upstream]; !exists {
			problems.add(routePath+".upstream", fmt.Sprintf("引用的 upstream %q 不存在", route.Upstream))
		}
	}

	// Router 是路由语法和歧义规则的唯一实现。Validate 调用同一个编译入口，确保
	// 非法 Host/Method/Path 和路由冲突在配置管线中、创建 Transport/listener 前被拒绝
	validateRouterConfiguration(problems, spec.Routes)

	if strings.TrimSpace(spec.Policies.RequestTimeout) == "" {
		problems.add("policies.request_timeout", "不能为空")
	} else if duration, err := time.ParseDuration(spec.Policies.RequestTimeout); err != nil {
		problems.add("policies.request_timeout", "必须是合法的 Go duration")
	} else if duration <= 0 {
		problems.add("policies.request_timeout", "必须为正数 duration")
	}
	if spec.Policies.Rate < 0 {
		problems.add("policies.rate", "必须为非负整数")
	}
	if spec.Policies.Burst < 0 {
		problems.add("policies.burst", "必须为非负整数")
	}

	return problems.err()
}

func validateEndpointURL(problems *validationProblems, field, rawURL string) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		problems.add(field, "必须是合法的绝对 URL")
		return
	}
	if parsed.User != nil {
		problems.add(field, "URL 不允许包含 userinfo")
	}
	if !parsed.IsAbs() || parsed.Host == "" {
		problems.add(field, "必须是包含 host 的绝对 URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		problems.add(field, "URL 协议必须为 http 或 https")
	}
}

// validateRouterConfiguration 使用生产 Router 编译器校验完整路由集合
// 这里不保留编译结果；Application 在装配时会再次编译并持有冻结树。启动阶段的少量
// 重复工作换取了 Config.Validate 作为公开入口时也无法绕过路由语法与冲突检查
func validateRouterConfiguration(problems *validationProblems, routes []RouteSpec) {
	inputs := make([]router.CompileInput, 0, len(routes))
	for _, route := range routes {
		inputs = append(inputs, router.CompileInput{
			RouteID:      route.ID,
			Host:         route.Host,
			Method:       route.Method,
			Path:         route.Path,
			Upstream:     route.Upstream,
			Priority:     route.Priority,
			PreserveHost: route.PreserveHost,
		})
	}
	if _, err := router.Compile(inputs); err != nil {
		problems.add("routes", err.Error())
	}
}
