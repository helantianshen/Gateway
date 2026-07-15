package router

import (
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// ParsePath 从请求 URL 解析出路径段，执行全部安全检查。
//
// 安全检查顺序（Oracle 建议的固定顺序）：
//  1. 取得 URL.EscapedPath()（不使用 URL.Path，后者已经解码，无法区分编码斜杠）
//  2. 扫描并验证所有 %XX 转义是否合法
//  3. 拒绝解码后为 / 或 \ 的转义（%2F、%5C、%2f、%5c）
//  4. 按字面 / 分段
//  5. 每段 PathUnescape
//  6. 验证 UTF-8 有效性
//  7. 拒绝解码后的 . 和 ..
//
// 返回解码后的路径段切片。任一检查失败时返回 MatchError（Code=MatchErrIllegalPath）。
func ParsePath(req *http.Request) ([]string, *MatchError) {
	if req == nil || req.URL == nil {
		return nil, &MatchError{Code: MatchErrIllegalPath, Message: "请求或 URL 为空"}
	}

	escaped := req.URL.EscapedPath()

	// 步骤 2：验证所有 %XX 转义合法
	if err := validatePercentEncoding(escaped); err != nil {
		return nil, err
	}

	// 步骤 3：拒绝解码后为 / 或 \ 的转义
	if err := rejectEncodedSeparators(escaped); err != nil {
		return nil, err
	}

	// 步骤 4：按字面 / 分段
	rawSegments := splitPath(escaped)

	// 步骤 5-7：逐段 PathUnescape、验证 UTF-8、拒绝 dot segment
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

// validatePercentEncoding 检查路径中所有 %XX 转义是否合法。
// 非法的 % 后面必须跟两个十六进制字符。
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

// rejectEncodedSeparators 拒绝解码后为 / 或 \ 的转义。
// %2F、%2f → /
// %5C、%5c → \
// 这些编码会改变分段语义，可能导致网关与 upstream 对路径的解释不一致。
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

// splitPath 按字面 / 分段，不去除空段。
// 空段表示重复斜杠（/a//b → ["a", "", "b"]），不自动合并。
// 开头的 / 产生一个空首段，会被去除。
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
