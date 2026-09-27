package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

var yamlLinePattern = regexp.MustCompile(`line ([0-9]+):\s*(.*)`)
var unknownFieldPattern = regexp.MustCompile(`field ([^ ]+) not found in type`)

// LoadConfig 严格解析单文档 YAML，不执行配置语义校验
// 第一遍保留行列位置并拒绝空值、多文档；第二遍用 KnownFields 拒绝未知字段
func LoadConfig(path string) (*ConfigSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %q 失败: %w", path, err)
	}

	var root yaml.Node
	nodeDecoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := nodeDecoder.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, yamlLocationError(path, 1, 1, "配置文件不能为空")
		}
		return nil, formatYAMLError(path, data, nil, err)
	}
	if isNullDocument(&root) {
		line, column := documentLocation(&root)
		return nil, yamlLocationError(path, line, column, "配置文档不能为 null")
	}

	// Decoder 必须在第一份文档后立即到达 EOF。即使第二份文档内容本身为 null
	// 它仍是多文档输入，必须拒绝，避免运行时只读取其中一部分而产生配置歧义
	var extra yaml.Node
	if err := nodeDecoder.Decode(&extra); err == nil {
		line, column := documentLocation(&extra)
		return nil, yamlLocationError(path, line, column, "只允许一个 YAML 文档")
	} else if !errors.Is(err, io.EOF) {
		return nil, formatYAMLError(path, data, nil, err)
	}

	// AST 已确认只有一份非空文档；第二遍才按 ConfigSpec 检查未知字段和类型
	var spec ConfigSpec
	strictDecoder := yaml.NewDecoder(bytes.NewReader(data))
	strictDecoder.KnownFields(true)
	if err := strictDecoder.Decode(&spec); err != nil {
		return nil, formatYAMLError(path, data, &root, err)
	}

	// 默认值需要区分“未填写”和解码后的零值，因而还要参考原始 YAML 节点
	// 当前判断只识别 endpoint 自身的直接字段，merge 字段也会被覆盖
	applyEndpointWeightDefaults(&spec, &root)
	return &spec, nil
}

// isNullDocument 同时识别显式 null（null、~）与只包含文档标记/注释的空文档
// yaml.v3 会把这些形式表示为 null scalar；单独判断文件字节是否为空无法覆盖它们
func isNullDocument(root *yaml.Node) bool {
	if root == nil || len(root.Content) == 0 {
		return true
	}
	node := root.Content[0]
	return node.Kind == yaml.ScalarNode && node.Tag == "!!null"
}

// documentLocation 返回文档中最有意义的起始位置。yaml.v3 对空文档可能不给出正数
// 行列，因此统一回退到 1:1，保证所有 YAML 边界错误都有稳定的路径、行号和列号
func documentLocation(root *yaml.Node) (int, int) {
	if root != nil {
		node := root
		if len(root.Content) > 0 {
			node = root.Content[0]
		}
		if node.Line > 0 && node.Column > 0 {
			return node.Line, node.Column
		}
	}
	return 1, 1
}

// applyEndpointWeightDefaults 对未直接声明 weight 的 endpoint 设置默认值
// YAML merge 引入的 weight 不被视为直接声明，目前也会被覆盖
func applyEndpointWeightDefaults(spec *ConfigSpec, root *yaml.Node) {
	top := documentMapping(root)
	upstreams := mappingValue(top, "upstreams")
	if upstreams == nil || upstreams.Kind != yaml.SequenceNode {
		return
	}
	for upstreamIndex, upstreamNode := range upstreams.Content {
		if upstreamIndex >= len(spec.Upstreams) || upstreamNode.Kind != yaml.MappingNode {
			continue
		}
		endpoints := mappingValue(upstreamNode, "endpoints")
		if endpoints == nil || endpoints.Kind != yaml.SequenceNode {
			continue
		}
		for endpointIndex, endpointNode := range endpoints.Content {
			if endpointIndex >= len(spec.Upstreams[upstreamIndex].Endpoints) || endpointNode.Kind != yaml.MappingNode {
				continue
			}
			if mappingValue(endpointNode, "weight") == nil {
				spec.Upstreams[upstreamIndex].Endpoints[endpointIndex].Weight = 100
			}
		}
	}
}

func documentMapping(root *yaml.Node) *yaml.Node {
	if root == nil || len(root.Content) == 0 {
		return nil
	}
	node := root.Content[0]
	if node.Kind != yaml.MappingNode {
		return nil
	}
	return node
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

// formatYAMLError 把 yaml.v3 的一条或多条解析错误转换为统一位置格式
// KnownFields 和类型错误通常包含精确行号但不直接暴露列号；此处利用第一遍保留的
// yaml.Node 查找对应 key/value 的列。若语法错误导致 AST 无法构建，则使用出错行上
// 第一个非空白字符作为可靠回退位置，仍保证调用方得到 path:line:column
func formatYAMLError(path string, data []byte, root *yaml.Node, err error) error {
	messages := splitYAMLError(err)
	formatted := make([]string, 0, len(messages))
	for _, message := range messages {
		line, detail := parseYAMLLine(message)
		if line <= 0 {
			line = 1
		}
		column := yamlErrorColumn(root, data, line, detail)
		if matches := unknownFieldPattern.FindStringSubmatch(detail); len(matches) == 2 {
			detail = fmt.Sprintf("未知字段 %q", matches[1])
		} else {
			detail = strings.TrimSpace(strings.TrimPrefix(detail, "yaml:"))
			if detail == "" {
				detail = "YAML 解析失败"
			}
		}
		formatted = append(formatted, fmt.Sprintf("%s:%d:%d: %s", path, line, column, detail))
	}
	return fmt.Errorf("配置 YAML 解析失败:\n  %s", strings.Join(formatted, "\n  "))
}

func splitYAMLError(err error) []string {
	text := strings.TrimSpace(err.Error())
	text = strings.TrimPrefix(text, "yaml: unmarshal errors:")
	parts := strings.Split(text, "\n")
	messages := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "yaml:")
		part = strings.TrimSpace(part)
		if part != "" {
			messages = append(messages, part)
		}
	}
	if len(messages) == 0 {
		return []string{"YAML 解析失败"}
	}
	return messages
}

func parseYAMLLine(message string) (int, string) {
	matches := yamlLinePattern.FindStringSubmatch(message)
	if len(matches) != 3 {
		return 1, message
	}
	line, err := strconv.Atoi(matches[1])
	if err != nil {
		return 1, matches[2]
	}
	return line, matches[2]
}

func yamlErrorColumn(root *yaml.Node, data []byte, line int, detail string) int {
	if root != nil {
		field := ""
		if matches := unknownFieldPattern.FindStringSubmatch(detail); len(matches) == 2 {
			field = matches[1]
		}
		if column := nodeColumnAtLine(root, line, field); column > 0 {
			return column
		}
	}
	return firstContentColumn(data, line)
}

// nodeColumnAtLine 对未知字段优先返回字段 key 的精确列；对类型错误则返回同一行最靠
// 右的 scalar 节点，通常就是无法解码的 value，而不是该 value 左侧的字段名
func nodeColumnAtLine(node *yaml.Node, line int, field string) int {
	if node == nil {
		return 0
	}
	best := 0
	if node.Line == line {
		if field != "" && node.Value == field {
			return node.Column
		}
		if field == "" && node.Kind == yaml.ScalarNode && node.Column > best {
			best = node.Column
		}
	}
	for _, child := range node.Content {
		column := nodeColumnAtLine(child, line, field)
		if field != "" && column > 0 {
			return column
		}
		if column > best {
			best = column
		}
	}
	return best
}

func firstContentColumn(data []byte, line int) int {
	lines := bytes.Split(data, []byte("\n"))
	if line <= 0 || line > len(lines) {
		return 1
	}
	for index, r := range string(lines[line-1]) {
		if !unicode.IsSpace(r) {
			return index + 1
		}
	}
	return 1
}

func yamlLocationError(path string, line, column int, message string) error {
	return fmt.Errorf("配置 YAML 解析失败:\n  %s:%d:%d: %s", path, line, column, message)
}
