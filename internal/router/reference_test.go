package router_test

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	router "github.com/helantianshen/gateway/internal/router"
)

// referenceMatcher 是差分测试使用的线性路由器。
//
// 它刻意不访问生产包的私有类型，也不调用生产实现的 Host、Method、Path 解析或
// specificity helper。每次 Match 遍历全部声明并收集所有候选，再独立选出唯一最优项。
type referenceMatcher struct {
	routes []referenceRoute
}

type referenceRoute struct {
	routeID      string
	upstreamID   string
	preserveHost bool
	priority     int
	hostKind     refHostKind
	hostValue    string
	method       string
	segments     []refSegment
}

type referenceResult struct {
	RouteID      string
	UpstreamID   string
	Params       map[string]string
	PreserveHost bool
}

type refHostKind uint8

const (
	refHostAny refHostKind = iota
	refHostWildcard
	refHostExact
)

type refSegmentKind uint8

const (
	refCatchAll refSegmentKind = iota + 1
	refParam
	refStatic
)

type refSegment struct {
	kind  refSegmentKind
	value string
}

func newReferenceMatcher(inputs []router.CompileInput) (*referenceMatcher, error) {
	routes := make([]referenceRoute, 0, len(inputs))
	for i, input := range inputs {
		kind, host, err := refParseHost(input.Host)
		if err != nil {
			return nil, fmt.Errorf("路由 %d host: %w", i, err)
		}
		method, err := refNormalizeMethod(input.Method)
		if err != nil {
			return nil, fmt.Errorf("路由 %d method: %w", i, err)
		}
		segments, err := refParsePath(input.Path)
		if err != nil {
			return nil, fmt.Errorf("路由 %d path: %w", i, err)
		}
		routes = append(routes, referenceRoute{
			routeID:      input.RouteID,
			upstreamID:   input.Upstream,
			preserveHost: input.PreserveHost,
			priority:     input.Priority,
			hostKind:     kind,
			hostValue:    host,
			method:       method,
			segments:     segments,
		})
	}
	return &referenceMatcher{routes: routes}, nil
}

type refCandidate struct {
	route      *referenceRoute
	methodRank int
	params     map[string]string
}

func (m *referenceMatcher) Match(host, method string, path []string) (*referenceResult, error) {
	normalizedHost := refNormalizeRequestHost(host)
	requestMethod := strings.ToUpper(method)
	candidates := make([]refCandidate, 0, len(m.routes))

	for i := range m.routes {
		route := &m.routes[i]
		if !refMatchHost(route, normalizedHost) {
			continue
		}
		methodRank, ok := refMatchMethod(route.method, requestMethod)
		if !ok {
			continue
		}
		params, ok := refMatchPath(route.segments, path)
		if !ok {
			continue
		}
		candidates = append(candidates, refCandidate{route: route, methodRank: methodRank, params: params})
	}

	if len(candidates) == 0 {
		return nil, nil
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return refCompareCandidates(candidates[i], candidates[j]) > 0
	})
	if len(candidates) > 1 && refCompareCandidates(candidates[0], candidates[1]) == 0 {
		return nil, fmt.Errorf("reference matcher 检测到歧义: %q 与 %q", candidates[0].route.routeID, candidates[1].route.routeID)
	}

	winner := candidates[0]
	return &referenceResult{
		RouteID:      winner.route.routeID,
		UpstreamID:   winner.route.upstreamID,
		Params:       winner.params,
		PreserveHost: winner.route.preserveHost,
	}, nil
}

func refParseHost(raw string) (refHostKind, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return refHostAny, "", nil
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x21 || raw[i] > 0x7f {
			return 0, "", fmt.Errorf("host 不是可见 ASCII")
		}
	}
	host, err := refSplitAuthority(raw)
	if err != nil {
		return 0, "", err
	}
	if strings.HasSuffix(host, "..") {
		return 0, "", fmt.Errorf("多个尾点")
	}
	host = refASCIILower(strings.TrimSuffix(host, "."))
	if strings.HasPrefix(host, "*.") {
		suffix := host[2:]
		if suffix == "" || strings.Contains(suffix, "*") {
			return 0, "", fmt.Errorf("非法 wildcard")
		}
		return refHostWildcard, suffix, nil
	}
	if strings.Contains(host, "*") {
		return 0, "", fmt.Errorf("非法 wildcard 位置")
	}
	return refHostExact, host, nil
}

func refNormalizeRequestHost(raw string) string {
	host, err := refSplitAuthority(raw)
	if err != nil {
		host = raw
	}
	return refASCIILower(strings.TrimSuffix(host, "."))
}

func refSplitAuthority(authority string) (string, error) {
	if authority == "" {
		return "", nil
	}
	if strings.HasPrefix(authority, "[") {
		end := strings.IndexByte(authority, ']')
		if end < 0 {
			return "", fmt.Errorf("缺少 IPv6 右方括号")
		}
		host := authority[1:end]
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() {
			return "", fmt.Errorf("非法 IPv6")
		}
		rest := authority[end+1:]
		if rest == "" {
			return host, nil
		}
		if len(rest) < 2 || rest[0] != ':' || !refValidPort(rest[1:]) {
			return "", fmt.Errorf("非法端口")
		}
		return host, nil
	}

	colonCount := strings.Count(authority, ":")
	if colonCount == 0 {
		return authority, nil
	}
	if colonCount == 1 {
		host, port, err := net.SplitHostPort(authority)
		if err != nil || host == "" || !refValidPort(port) {
			return "", fmt.Errorf("非法 host:port")
		}
		return host, nil
	}
	addr, err := netip.ParseAddr(authority)
	if err != nil || !addr.Is6() {
		return "", fmt.Errorf("非法裸 IPv6")
	}
	return authority, nil
}

func refValidPort(raw string) bool {
	port, err := strconv.ParseUint(raw, 10, 16)
	return err == nil && port > 0
}

func refASCIILower(value string) string {
	bytes := []byte(value)
	for i, c := range bytes {
		if c >= 'A' && c <= 'Z' {
			bytes[i] += 'a' - 'A'
		}
	}
	return string(bytes)
}

func refMatchHost(route *referenceRoute, request string) bool {
	switch route.hostKind {
	case refHostAny:
		return true
	case refHostExact:
		return request == route.hostValue
	case refHostWildcard:
		suffix := "." + route.hostValue
		if !strings.HasSuffix(request, suffix) {
			return false
		}
		prefix := strings.TrimSuffix(request, suffix)
		return prefix != "" && !strings.Contains(prefix, ".")
	default:
		return false
	}
}

func refNormalizeMethod(raw string) (string, error) {
	method := strings.TrimSpace(raw)
	for i := 0; i < len(method); i++ {
		c := method[i]
		valid := c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))
		if !valid {
			return "", fmt.Errorf("非法 method token")
		}
	}
	return strings.ToUpper(method), nil
}

// refMatchMethod 返回请求态 specificity：显式 HEAD=2、其他显式或 GET fallback=1、any=0。
func refMatchMethod(routeMethod, requestMethod string) (int, bool) {
	if routeMethod == "" {
		return 0, true
	}
	if routeMethod == requestMethod {
		if requestMethod == "HEAD" {
			return 2, true
		}
		return 1, true
	}
	if requestMethod == "HEAD" && routeMethod == "GET" {
		return 1, true
	}
	return 0, false
}

func refParsePath(pattern string) ([]refSegment, error) {
	if pattern == "" {
		return nil, fmt.Errorf("path 不能为空")
	}
	pattern = strings.TrimPrefix(pattern, "/")
	if pattern == "" {
		return nil, nil
	}
	rawSegments := strings.Split(pattern, "/")
	segments := make([]refSegment, 0, len(rawSegments))
	names := make(map[string]struct{})
	for i, raw := range rawSegments {
		if raw == "" {
			return nil, fmt.Errorf("空路径段")
		}
		segment := refSegment{kind: refStatic, value: raw}
		if strings.HasPrefix(raw, ":") {
			segment = refSegment{kind: refParam, value: raw[1:]}
		} else if strings.HasPrefix(raw, "*") {
			segment = refSegment{kind: refCatchAll, value: raw[1:]}
			if i != len(rawSegments)-1 {
				return nil, fmt.Errorf("catch-all 必须位于末尾")
			}
		}
		if segment.kind != refStatic {
			if segment.value == "" {
				return nil, fmt.Errorf("参数名为空")
			}
			if _, exists := names[segment.value]; exists {
				return nil, fmt.Errorf("参数名重复")
			}
			names[segment.value] = struct{}{}
		}
		segments = append(segments, segment)
	}
	return segments, nil
}

func refMatchPath(pattern []refSegment, path []string) (map[string]string, bool) {
	params := make(map[string]string)
	position := 0
	for _, segment := range pattern {
		switch segment.kind {
		case refStatic:
			if position >= len(path) || path[position] != segment.value {
				return nil, false
			}
			position++
		case refParam:
			if position >= len(path) || path[position] == "" {
				return nil, false
			}
			params[segment.value] = path[position]
			position++
		case refCatchAll:
			params[segment.value] = strings.Join(path[position:], "/")
			return params, true
		}
	}
	return params, position == len(path)
}

func refCompareCandidates(a, b refCandidate) int {
	if diff := int(a.route.hostKind) - int(b.route.hostKind); diff != 0 {
		return diff
	}
	if diff := a.methodRank - b.methodRank; diff != 0 {
		return diff
	}
	if diff := refComparePath(a.route.segments, b.route.segments); diff != 0 {
		return diff
	}
	return a.route.priority - b.route.priority
}

// refComparePath 逐段比较 static > param > catch-all。若共同前缀相同且一个模式
// 已精确结束，另一个只靠空 catch-all 命中，则精确结束更具体。
func refComparePath(a, b []refSegment) int {
	maxLen := len(a)
	if len(b) > maxLen {
		maxLen = len(b)
	}
	for i := 0; i < maxLen; i++ {
		if i >= len(a) {
			return 1
		}
		if i >= len(b) {
			return -1
		}
		if diff := int(a[i].kind) - int(b[i].kind); diff != 0 {
			return diff
		}
	}
	return 0
}
