package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

const phase2Unsupported = "Phase 2 暂不支持，请等待 Phase 3/4"

// validationProblems 聚合一份配置中的全部结构、语义和当前阶段约束错误。
// 错误项只携带文件路径与稳定字段路径，不伪造 YAML 行号；行列定位仅属于 loader
// 能够可靠提供的语法/schema 错误。聚合后一次返回可显著减少配置修复的启动轮次。
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

// Validate 对 ConfigSpec 执行结构校验、跨字段语义校验和 Phase 2 运行约束校验。
//
// URL 错误有意不包含用户填写的完整原始值，因为 URL 可能意外携带凭据或 token。
// route/upstream ID 则属于配置中的非敏感标识符，可以在引用错误中安全回显，以帮助
// 操作者定位拼写问题。
func Validate(spec *ConfigSpec, file string) error {
	problems := &validationProblems{file: file}
	if spec == nil {
		problems.add("", "配置对象不能为空")
		return problems.err()
	}

	if spec.APIVersion != "v1" {
		problems.add("api_version", "必须为 \"v1\"")
	}

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

	validatePhase2Constraints(problems, spec, upstreamIndexes)
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

// validatePhase2Constraints 明确拒绝 schema 已能表达、但当前运行时尚不能正确执行的
// 拓扑。绝不能只取第一条 route 或第一个 endpoint 继续启动，否则配置看似生效，实际
// 流量行为却与声明不一致。每条错误都包含统一的阶段迁移提示。
func validatePhase2Constraints(problems *validationProblems, spec *ConfigSpec, upstreamIndexes map[string]int) {
	if len(spec.Routes) != 1 {
		problems.add("编译约束", fmt.Sprintf("%s：必须恰好配置一条 catch-all route", phase2Unsupported))
		return
	}

	route := spec.Routes[0]
	if route.Path != "/" {
		problems.add("routes[0].path", fmt.Sprintf("%s：当前只支持 catch-all path \"/\"", phase2Unsupported))
	}
	if route.Host != "" {
		problems.add("routes[0].host", fmt.Sprintf("%s：host 匹配将在 Phase 3 实现", phase2Unsupported))
	}
	if route.Method != "" {
		problems.add("routes[0].method", fmt.Sprintf("%s：method 匹配将在 Phase 3 实现", phase2Unsupported))
	}
	if route.Priority != 0 {
		problems.add("routes[0].priority", fmt.Sprintf("%s：priority 调度将在 Phase 3 实现", phase2Unsupported))
	}
	if route.PreserveHost {
		problems.add("routes[0].preserve_host", fmt.Sprintf("%s：preserve_host 将在 Phase 3 实现", phase2Unsupported))
	}

	if upstreamIndex, exists := upstreamIndexes[route.Upstream]; exists {
		if endpointCount := len(spec.Upstreams[upstreamIndex].Endpoints); endpointCount != 1 {
			problems.add(
				fmt.Sprintf("upstreams[%d].endpoints", upstreamIndex),
				fmt.Sprintf("%s：route 引用的 upstream 必须恰好包含一个 endpoint", phase2Unsupported),
			)
		}
	}
}
