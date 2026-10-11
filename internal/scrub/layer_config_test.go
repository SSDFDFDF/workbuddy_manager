package scrub

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAgentSDKIdentity 测试 #3b Claude Agent SDK 身份句被正确改写
func TestAgentSDKIdentity(t *testing.T) {
	in := "You are a Claude agent, built on Anthropic's Claude Agent SDK, and ready to assist."
	out := Text(in)
	if strings.Contains(out, "built on Anthropic's Claude Agent SDK,") {
		t.Errorf("Agent SDK 身份句未被改写: %s", out)
	}
	if !strings.Contains(out, "Claude Agent SDK tool") {
		t.Errorf("未包含预期的改写文本: %s", out)
	}
}

// TestToolsChannelCoverage 测试 tools 声明中的描述与属性标题被改写
func TestToolsChannelCoverage(t *testing.T) {
	obj := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "search",
					"description": "Execute search, reports 11128 if error",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"query": map[string]any{
								"type":        "string",
								"description": ccIdentity,
								"title":       "cc_version=1.0; query input",
							},
						},
					},
				},
			},
		},
	}

	l, err := BuildLayer(true, nil, nil, nil)
	if err != nil {
		t.Fatalf("BuildLayer failed: %v", err)
	}

	raw, _ := json.Marshal(obj)
	if !l.Dirty(raw) {
		t.Fatal("tools 中的指纹应触发 Dirty")
	}

	if !l.Object(obj) {
		t.Fatal("Object 应报告改动")
	}

	res, _ := json.Marshal(obj)
	s := string(res)

	if strings.Contains(s, "11128") {
		t.Errorf("tools function description 中的 11128 未被改写: %s", s)
	}
	if !strings.Contains(s, "11-128") {
		t.Errorf("tools function description 未改写为 11-128: %s", s)
	}
	if strings.Contains(s, "Anthropic's official CLI for Claude") {
		t.Errorf("tools property description 未被改写: %s", s)
	}
	if strings.Contains(s, "cc_version") {
		t.Errorf("tools property title 未被改写: %s", s)
	}
}

// TestBuildLayerDisabled 测试按 ID 禁用内置规则
func TestBuildLayerDisabled(t *testing.T) {
	// 禁用 err-11128 与 identity-claude-code
	l, err := BuildLayer(true, []string{"err-11128", "identity-claude-code", "hdr-billing"}, nil, nil)
	if err != nil {
		t.Fatalf("BuildLayer: %v", err)
	}

	// 11128 应被保留（不改写）
	o1 := map[string]any{"messages": []any{map[string]any{"content": "code=11128"}}}
	l.Object(o1)
	s1, _ := json.Marshal(o1)
	if !strings.Contains(string(s1), "11128") {
		t.Errorf("err-11128 被禁用后不应被改写: %s", s1)
	}

	// identity-claude-code 应被保留（不改写）
	o2 := map[string]any{"messages": []any{map[string]any{"content": ccIdentity}}}
	l.Object(o2)
	s2, _ := json.Marshal(o2)
	if !strings.Contains(string(s2), "official CLI for Claude.") {
		t.Errorf("identity-claude-code 被禁用后不应被改写: %s", s2)
	}

	// hdr-billing 应被保留（不整段删除）
	o3 := map[string]any{"messages": []any{map[string]any{"content": ccHeader}}}
	l.Object(o3)
	s3, _ := json.Marshal(o3)
	if !strings.Contains(string(s3), "x-anthropic-billing-header") {
		t.Errorf("hdr-billing 被禁用后不应被删除: %s", s3)
	}

	// 未禁用的 pr-branch 仍应正常改写
	o4 := map[string]any{"messages": []any{map[string]any{"content": ccBranch}}}
	l.Object(o4)
	s4, _ := json.Marshal(o4)
	if strings.Contains(string(s4), "Main branch (") {
		t.Errorf("未禁用的 pr-branch 应正常改写: %s", s4)
	}
}

// TestBuildLayerOverrides 测试替换词覆盖
func TestBuildLayerOverrides(t *testing.T) {
	overrides := map[string]string{
		"identity-claude-code": "You are Claude Code, an unofficial assistant",
		"err-11128":            "11_128",
		"hdr-billing-bare":     "custom-hdr",
	}
	l, err := BuildLayer(true, nil, overrides, nil)
	if err != nil {
		t.Fatalf("BuildLayer: %v", err)
	}

	// identity-claude-code 覆盖生效
	o1 := map[string]any{"messages": []any{map[string]any{"content": ccIdentity}}}
	l.Object(o1)
	s1, _ := json.Marshal(o1)
	if !strings.Contains(string(s1), "an unofficial assistant") {
		t.Errorf("覆盖后的替换词未生效: %s", s1)
	}

	// err-11128 覆盖生效
	o2 := map[string]any{"messages": []any{map[string]any{"content": "code=11128"}}}
	l.Object(o2)
	s2, _ := json.Marshal(o2)
	if !strings.Contains(string(s2), "11_128") {
		t.Errorf("覆盖后的 11_128 未生效: %s", s2)
	}

	// hdr-billing-bare 覆盖生效
	o3 := map[string]any{"messages": []any{map[string]any{"content": "see `x-anthropic-billing-header`"}}}
	l.Object(o3)
	s3, _ := json.Marshal(o3)
	if !strings.Contains(string(s3), "custom-hdr") {
		t.Errorf("覆盖后的 custom-hdr 未生效: %s", s3)
	}
}

// TestBuildLayerEmptyOverrideFallsBackToDefault 空串覆盖 = 用内置默认。
// 面板清空输入框提交空串时，行为必须与从没配过该项一致（而不是改成“删除匹配串”）。
func TestBuildLayerEmptyOverrideFallsBackToDefault(t *testing.T) {
	l, err := BuildLayer(true, nil, map[string]string{"identity-claude-code": ""}, nil)
	if err != nil {
		t.Fatalf("BuildLayer: %v", err)
	}
	o := map[string]any{"messages": []any{map[string]any{"content": ccIdentity}}}
	l.Object(o)
	s, _ := json.Marshal(o)
	if !strings.Contains(string(s), "official CLI tool for Claude") {
		t.Errorf("空串覆盖应回落内置默认改写: %s", s)
	}
	if strings.Contains(string(s), "Anthropic's official CLI for Claude.") {
		t.Errorf("空串覆盖不得停用规则: %s", s)
	}
}

// TestBuildLayerBuiltinThenCustomCascade 顺序语义：内置先改，自定义在内置结果上
// 继续改（可级联）。历史实现是两次字段遍历，合并成单遍遍历时不能把级联一并丢掉。
func TestBuildLayerBuiltinThenCustomCascade(t *testing.T) {
	// 自定义规则的匹配串只在**内置改写之后**才出现（11128 → 11-128 → 11_128）。
	l, err := BuildLayer(true, nil, nil, []Rule{{Match: "11-128", Replace: "11_128"}})
	if err != nil {
		t.Fatalf("BuildLayer: %v", err)
	}
	o := map[string]any{"messages": []any{map[string]any{"content": "upstream said code=11128"}}}
	l.Object(o)
	s, _ := json.Marshal(o)
	if !strings.Contains(string(s), "11_128") {
		t.Errorf("自定义规则应能匹配内置改写结果（级联）: %s", s)
	}
	if strings.Contains(string(s), "11128") {
		t.Errorf("原始指纹未清除: %s", s)
	}
}
