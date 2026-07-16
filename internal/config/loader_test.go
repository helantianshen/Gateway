package config

import (
	"regexp"
	"strings"
	"testing"
)

func TestLoadConfig_ValidAndDefaultWeight(t *testing.T) {
	path := writeConfigFile(t, strings.Replace(validConfigYAML, "        weight: 100\n", "", 1))

	spec, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig 返回意外错误: %v", err)
	}
	if spec.APIVersion != "v1" || len(spec.Upstreams) != 1 || len(spec.Routes) != 1 {
		t.Fatalf("配置模型解码不完整: %+v", spec)
	}
	if weight := spec.Upstreams[0].Endpoints[0].Weight; weight != 100 {
		t.Errorf("省略 weight 后值 = %d, want 默认值 100", weight)
	}
}

func TestLoadConfig_ExplicitZeroWeightIsNotDefaulted(t *testing.T) {
	path := writeConfigFile(t, strings.Replace(validConfigYAML, "weight: 100", "weight: 0", 1))
	spec, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig 返回意外错误: %v", err)
	}
	if spec.Upstreams[0].Endpoints[0].Weight != 0 {
		t.Fatalf("显式 weight: 0 被错误替换为默认值: %+v", spec.Upstreams[0].Endpoints[0])
	}
	if err := Validate(spec, path); err == nil || !strings.Contains(err.Error(), ".weight: 必须为正整数") {
		t.Fatalf("显式 weight: 0 校验错误 = %v", err)
	}
}

func TestLoadConfig_KnownFieldsReportsPathLineAndColumn(t *testing.T) {
	content := strings.Replace(validConfigYAML, "api_version: v1", "api_version: v1\nunknown_option: true", 1)
	path := writeConfigFile(t, content)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("未知字段未被 KnownFields 拒绝")
	}
	quotedPath := regexp.QuoteMeta(path)
	if matched, _ := regexp.MatchString(quotedPath+`:[0-9]+:[0-9]+: 未知字段 "unknown_option"`, err.Error()); !matched {
		t.Fatalf("未知字段错误没有路径、行号和列号: %v", err)
	}
}

func TestLoadConfig_AggregatesKnownFieldErrors(t *testing.T) {
	content := strings.Replace(validConfigYAML, "api_version: v1", "api_version: v1\nfirst_unknown: true\nsecond_unknown: true", 1)
	path := writeConfigFile(t, content)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("多个未知字段未被拒绝")
	}
	for _, field := range []string{"first_unknown", "second_unknown"} {
		if !strings.Contains(err.Error(), "未知字段 \""+field+"\"") {
			t.Errorf("聚合错误未包含 %s: %v", field, err)
		}
	}
}

func TestLoadConfig_SyntaxAndTypeErrorsHaveLocation(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "语法错误", content: "api_version: v1\nupstreams: [\n"},
		{name: "类型错误", content: strings.Replace(validConfigYAML, "weight: 100", "weight: heavy", 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfigFile(t, tt.content)
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatal("非法 YAML 未返回错误")
			}
			matched, matchErr := regexp.MatchString(regexp.QuoteMeta(path)+`:[0-9]+:[0-9]+:`, err.Error())
			if matchErr != nil || !matched {
				t.Fatalf("错误没有路径、行号和列号: %v", err)
			}
		})
	}
}

func TestLoadConfig_RejectsEmptyNullAndMultipleDocuments(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "空文件", content: "", want: "配置文件不能为空"},
		{name: "只有注释", content: "# comment only\n", want: "配置文件不能为空"},
		{name: "null", content: "null\n", want: "配置文档不能为 null"},
		{name: "波浪号 null", content: "~\n", want: "配置文档不能为 null"},
		{name: "多文档", content: validConfigYAML + "---\napi_version: v1\n", want: "只允许一个 YAML 文档"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfigFile(t, tt.content)
			_, err := LoadConfig(path)
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadConfig 错误 = %v, want 包含路径 %q 和 %q", err, path, tt.want)
			}
		})
	}
}
