package router

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// NormalizeHost 去除端口和单个尾点，并将 ASCII 字母转为小写
// 空输入表示没有 authority，只能进入 any-host；非空非法输入返回独立错误
// 请求沿用配置的 DNS/IP 子集约束，不接受 URI reg-name 的任意字符
func NormalizeHost(rawHost string) (string, *MatchError) {
	if rawHost == "" {
		return "", nil
	}
	hostname, err := splitAuthorityHost(rawHost)
	if err != nil {
		return "", &MatchError{Code: MatchErrIllegalHost, Message: "Host authority 格式非法"}
	}
	normalized := normalizeHostname(hostname)
	// authority 拆分已验证含冒号的 IPv6；IPv4 的数字与点也满足下方 DNS 字符校验
	if strings.ContainsRune(normalized, ':') {
		return normalized, nil
	}
	if validateDNSName(normalized) != nil {
		return "", &MatchError{Code: MatchErrIllegalHost, Message: "Host 不是合法 DNS 名称或 IP 地址"}
	}
	return normalized, nil
}

// ParseHostPattern 解析配置中的 host 字段
//
// 支持空 Host、exact Host 和前缀形式的单层 wildcard（*.example.com）
// 配置侧严格拒绝非 ASCII、非法端口、多个尾点、空 DNS label 及非前缀形式的 *
// 确保所有归一化在启动阶段完成，请求热路径只做确定性比较
func ParseHostPattern(raw string) (hostPattern, error) {
	// 配置侧严格解析：空值是任意 Host，非空值必须能在启动时确定匹配类型
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

	// wildcard 仅允许最前面的 "*."，后缀必须是 DNS 名称而不能是 IP
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

	// 非 wildcard 的模式可用合法 DNS 名称或 IP 地址精确匹配
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

// splitAuthorityHost 从配置或请求 authority 中提取 hostname，并验证显式端口
// IPv6 可使用 [::1]、[::1]:8080 或裸地址 ::1；带端口的 IPv6 必须使用括号
func splitAuthorityHost(authority string) (string, error) {
	if authority == "" {
		return "", nil
	}
	// 带方括号的 IPv6 可以携带端口；端口部分始终单独校验
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

	// 一个冒号按 host:port 处理；多个冒号只接受不带端口的裸 IPv6
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
	// 常见的小写 Host 直接复用输入，仅在出现 ASCII 大写时创建副本
	for i := 0; i < len(hostname); i++ {
		if hostname[i] < 'A' || hostname[i] > 'Z' {
			continue
		}
		bytes := []byte(hostname)
		for j := i; j < len(bytes); j++ {
			if bytes[j] >= 'A' && bytes[j] <= 'Z' {
				bytes[j] += 'a' - 'A'
			}
		}
		return string(bytes)
	}
	return hostname
}

func validateDNSName(hostname string) error {
	if len(hostname) > 253 {
		return fmt.Errorf("DNS host 长度不能超过 253 字节")
	}
	labelStart := 0
	for index := 0; index <= len(hostname); index++ {
		if index < len(hostname) && hostname[index] != '.' {
			continue
		}
		label := hostname[labelStart:index]
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
		labelStart = index + 1
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

// MatchHost 检查归一化后的请求 host 是否匹配此 hostPattern
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

// hostParseError 表示 host 模式解析错误
type hostParseError struct {
	input  string
	reason string
}

// Error 返回包含原始 Host 和拒绝原因的配置错误
func (e *hostParseError) Error() string {
	return "host 模式解析失败 (" + e.input + "): " + e.reason
}

// Specificity 返回 host 模式的排序等级：exact 高于 wildcard，高于 any
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
