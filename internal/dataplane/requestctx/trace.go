package requestctx

import "strings"

// TraceIDFromTraceParent 从 Phase 5 支持的 W3C traceparent v00 中提取 trace ID。
// 本阶段只做受控解析，不创建 Span，也不生成新的 Trace。
func TraceIDFromTraceParent(traceParent string) (string, bool) {
	if len(traceParent) != 55 ||
		traceParent[2] != '-' ||
		traceParent[35] != '-' ||
		traceParent[52] != '-' ||
		traceParent[:2] != "00" {
		return "", false
	}

	traceID := traceParent[3:35]
	parentID := traceParent[36:52]
	flags := traceParent[53:55]
	if !isLowerHex(traceID) || !isLowerHex(parentID) || !isLowerHex(flags) {
		return "", false
	}
	if allZero(traceID) || allZero(parentID) {
		return "", false
	}
	return traceID, true
}

func isLowerHex(value string) bool {
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') {
			continue
		}
		return false
	}
	return true
}

func allZero(value string) bool {
	return strings.Trim(value, "0") == ""
}
