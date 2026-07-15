package router

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// NormalizeHost 归一化请求 Host 头，用于路由匹配。
//
// 归一化顺序：
//  1. 按 authority 语法安全拆分 hostname/port，兼容带括号和不带括号的 IPv6；
//  2. 去除 hostname 的单个 trailing dot；
//  3. 仅执行 ASCII lowercase。
//
// 请求侧采用保守的容错策略：authority 非法时保留原值（只做单尾点和 ASCII
// lowercase），使其无法意外命中合法 exact/wildcard 配置；任意 Host 路由仍可接收它。
func NormalizeHost(rawHost string) string {
	if rawHost == "" {
		return ""
	}
	hostname, err := splitAuthorityHost(rawHost)
	if err != nil {
		hostname = rawHost
	}
	return normalizeHostname(hostname)
}

// ParseHostPattern 解析配置中的 host 字段。
//
// 支持空 Host、exact Host 和前缀形式的单层 wildcard（*.example.com）。配置侧
// 严格拒绝非 ASCII、非法端口、多个尾点、空 DNS label 及非前缀形式的 *，确保
// 所有归一化在启动阶段完成，请求热路径只做确定性比较。
func ParseHostPattern(raw string) (hostPattern, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return hostPattern{kind: hostAny}, nil
	}
	if !isASCII(raw) {
		return hostPattern{}, &hostParseError{input: raw, reason: "host 必须只包含 ASCII 字符"}
	}

	hostname, err := splitAuthorityHost(raw)
	if err != nil {
		return hostPattern{}, &hostParseError{input: raw, reason: err.Error()}
	}
	if strings.HasSuffix(hostname, "..") {
		return hostPattern{}, &hostParseError{input: raw, reason: "host 不能包含多个尾点"}
	}
	normalized := normalizeHostname(hostname)
	if normalized == "" {
		return hostPattern{}, &hostParseError{input: raw, reason: "hostname 不能为空"}
	}

	if strings.HasPrefix(normalized, "*.") {
		suffix := normalized[2:]
		if suffix == "" {
			return hostPattern{}, &hostParseError{input: raw, reason: "通配 host 的域名后缀不能为空"}
		}
		if strings.Contains(suffix, "*") {
			return hostPattern{}, &hostParseError{input: raw, reason: "通配 * 只允许出现在 host 开头"}
		}
		if net.ParseIP(suffix) != nil {
			return hostPattern{}, &hostParseError{input: raw, reason: "通配 host 的后缀不能是 IP 地址"}
		}
		if err := validateDNSName(suffix); err != nil {
			return hostPattern{}, &hostParseError{input: raw, reason: err.Error()}
		}
		return hostPattern{kind: hostWildcard, value: suffix}, nil
	}

	if strings.Contains(normalized, "*") {
		return hostPattern{}, &hostParseError{input: raw, reason: "非通配 host 不允许包含 *"}
	}
	if _, err := netip.ParseAddr(normalized); err != nil {
		if err := validateDNSName(normalized); err != nil {
			return hostPattern{}, &hostParseError{input: raw, reason: err.Error()}
		}
	}
	return hostPattern{kind: hostExact, value: normalized}, nil
}

// splitAuthorityHost 从配置或请求 authority 中提取 hostname，并验证显式端口。
// IPv6 可使用 [::1]、[::1]:8080 或裸地址 ::1；带端口的 IPv6 必须使用括号。
func splitAuthorityHost(authority string) (string, error) {
	if authority == "" {
		return "", nil
	}
	if strings.HasPrefix(authority, "[") {
		end := strings.IndexByte(authority, ']')
		if end < 0 {
			return "", fmt.Errorf("IPv6 host 缺少右方括号")
		}
		hostname := authority[1:end]
		if addr, err := netip.ParseAddr(hostname); err != nil || !addr.Is6() {
			return "", fmt.Errorf("方括号内必须是合法 IPv6 地址")
		}
		rest := authority[end+1:]
		if rest == "" {
			return hostname, nil
		}
		if !strings.HasPrefix(rest, ":") || len(rest) == 1 {
			return "", fmt.Errorf("IPv6 host 后的端口格式非法")
		}
		if err := validatePort(rest[1:]); err != nil {
			return "", err
		}
		return hostname, nil
	}

	switch strings.Count(authority, ":") {
	case 0:
		return authority, nil
	case 1:
		hostname, port, err := net.SplitHostPort(authority)
		if err != nil || hostname == "" {
			return "", fmt.Errorf("host:port 格式非法")
		}
		if err := validatePort(port); err != nil {
			return "", err
		}
		return hostname, nil
	default:
		addr, err := netip.ParseAddr(authority)
		if err != nil || !addr.Is6() {
			return "", fmt.Errorf("带端口的 IPv6 host 必须使用方括号")
		}
		return authority, nil
	}
}

func validatePort(raw string) error {
	port, err := strconv.ParseUint(raw, 10, 16)
	if err != nil || port == 0 {
		return fmt.Errorf("端口必须是 1 到 65535 的十进制整数")
	}
	return nil
}

func normalizeHostname(hostname string) string {
	hostname = strings.TrimSuffix(hostname, ".")
	bytes := []byte(hostname)
	for i, c := range bytes {
		if c >= 'A' && c <= 'Z' {
			bytes[i] = c + ('a' - 'A')
		}
	}
	return string(bytes)
}

func validateDNSName(hostname string) error {
	if len(hostname) > 253 {
		return fmt.Errorf("DNS host 长度不能超过 253 字节")
	}
	for _, label := range strings.Split(hostname, ".") {
		if label == "" {
			return fmt.Errorf("DNS host 不能包含空 label")
		}
		if len(label) > 63 {
			return fmt.Errorf("DNS label 长度不能超过 63 字节")
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
				continue
			}
			return fmt.Errorf("DNS host 包含非法字符")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("DNS label 不能以连字符开头或结尾")
		}
	}
	return nil
}

func isASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] > 0x7f || value[i] < 0x21 {
			return false
		}
	}
	return true
}

// MatchHost 检查归一化后的请求 host 是否匹配此 hostPattern。
func (p hostPattern) MatchHost(requestHost string) bool {
	switch p.kind {
	case hostAny:
		return true
	case hostExact:
		return requestHost == p.value
	case hostWildcard:
		if !strings.HasSuffix(requestHost, "."+p.value) {
			return false
		}
		prefix := strings.TrimSuffix(requestHost, "."+p.value)
		return prefix != "" && !strings.Contains(prefix, ".")
	default:
		return false
	}
}

// hostParseError 表示 host 模式解析错误。
type hostParseError struct {
	input  string
	reason string
}

func (e *hostParseError) Error() string {
	return "host 模式解析失败 (" + e.input + "): " + e.reason
}

// Specificity 返回 host 模式的 specificity 值，用于编译排序与冲突判断。
func (p hostPattern) Specificity() int {
	switch p.kind {
	case hostExact:
		return 2
	case hostWildcard:
		return 1
	default:
		return 0
	}
}
