package router

import (
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// ParsePath 从 EscapedPath 按段解码，保留原始斜杠边界
// 编码斜杠、反斜杠、dot segment、无效转义或 UTF-8 返回 MatchErrIllegalPath
func ParsePath(req *http.Request) ([]string, *MatchError) {
	if req == nil || req.URL == nil {
		return nil, &MatchError{Code: MatchErrIllegalPath, Message: "请求或 URL 为空"}
	}

	escaped := req.URL.EscapedPath()

	// 先检查转义语法和编码分隔符，再做任何路径段解码
	// 否则 %2F 或 %5C 可能在网关与上游之间产生不同的路径层级
	if err := validatePercentEncoding(escaped); err != nil {
		return nil, err
	}

	if err := rejectEncodedSeparators(escaped); err != nil {
		return nil, err
	}

	// 按原始斜杠分段后再逐段解码，保留重复斜杠和尾斜杠的区别
	rawSegments := splitPath(escaped)

	// 每段检查 UTF-8 与 dot segment；不会自动清理或重定向请求路径
	segments := make([]string, 0, len(rawSegments))
	for _, raw := range rawSegments {
		decoded, err := url.PathUnescape(raw)
		if err != nil {
			return nil, &MatchError{Code: MatchErrIllegalPath, Message: "路径段解码失败: " + err.Error()}
		}
		if !utf8.ValidString(decoded) {
			return nil, &MatchError{Code: MatchErrIllegalPath, Message: "路径段不是有效 UTF-8"}
		}
		if decoded == "." || decoded == ".." {
			return nil, &MatchError{Code: MatchErrIllegalPath, Message: "路径包含 dot segment"}
		}
		segments = append(segments, decoded)
	}

	return segments, nil
}

// validatePercentEncoding 检查路径中所有 %XX 转义是否合法
// 非法的 % 后面必须跟两个十六进制字符
func validatePercentEncoding(path string) *MatchError {
	for i := 0; i < len(path); i++ {
		if path[i] != '%' {
			continue
		}
		// 检查 % 后是否有足够字符
		if i+2 >= len(path) {
			return &MatchError{Code: MatchErrIllegalPath, Message: "不完整的百分号转义"}
		}
		hex := path[i+1 : i+3]
		if !isHex(hex[0]) || !isHex(hex[1]) {
			return &MatchError{Code: MatchErrIllegalPath, Message: "无效的百分号转义"}
		}
		i += 2
	}
	return nil
}

// rejectEncodedSeparators 拒绝编码的斜杠和反斜杠，避免网关与上游的分段语义不一致
func rejectEncodedSeparators(path string) *MatchError {
	lower := strings.ToLower(path)
	if strings.Contains(lower, "%2f") {
		return &MatchError{Code: MatchErrIllegalPath, Message: "路径包含编码斜杠 %2F"}
	}
	if strings.Contains(lower, "%5c") {
		return &MatchError{Code: MatchErrIllegalPath, Message: "路径包含编码反斜杠 %5C"}
	}
	return nil
}

// splitPath 按字面 / 分段，不去除空段
// 空段表示重复斜杠（/a//b → ["a", "", "b"]），不自动合并
// 开头的 / 产生一个空首段，会被去除
func splitPath(path string) []string {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
