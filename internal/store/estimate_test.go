package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestEstimateInputTokensIsCalibratedToRealTokenizers 校准估算：
//   - 中日韩等全角字符按 1.25 token/字（真实约 1~1.5）；
//   - 拉丁/符号按约 3.5 字符/token（真实约 3.5~4）；
//   - messages 的 JSON 只计一份，不再把整段 JSON 与每条消息各算一遍。
//
// 断言以「消息 JSON 的字符数」为基准给出区间，避免随分词细节波动。
func TestEstimateInputTokensIsCalibratedToRealTokenizers(t *testing.T) {
	cases := []struct {
		name    string
		content string
		lo, hi  float64 // 每字符的 token 区间
	}{
		{"英文短句", "prompt text", 0.25, 0.35},
		{"中文 1000 字", strings.Repeat("测", 1000), 1.1, 1.4},
		{"英文 4000 字符", strings.Repeat("a", 4000), 0.25, 0.35},
		{"中英混合", strings.Repeat("测", 500) + strings.Repeat("a", 2000), 0.25, 1.4},
	}
	for _, tc := range cases {
		msg := `[{"role":"user","content":"` + tc.content + `"}]`
		runes := float64(utf8.RuneCountInString(msg))
		got := estimateInputTokens(ReserveInput{InputText: msg, StringFields: []string{msg}})
		if float64(got) < runes*tc.lo || float64(got) > runes*tc.hi {
			t.Fatalf("%s: estimate = %d, want within [%.0f, %.0f]（消息 %d 字符）",
				tc.name, got, runes*tc.lo, runes*tc.hi, int(runes))
		}
	}
}

// TestEstimateInputTokensWeightsCJKAboveLatin 同样长度的中文比英文贵约 4 倍，
// 这是校准的主要目的：中文按字符计、英文按 3.5 字符计。
func TestEstimateInputTokensWeightsCJKAboveLatin(t *testing.T) {
	cjk := estimateInputTokens(ReserveInput{StringFields: []string{strings.Repeat("测", 1000)}})
	latin := estimateInputTokens(ReserveInput{StringFields: []string{strings.Repeat("a", 1000)}})
	if cjk < latin*3 || cjk > latin*6 {
		t.Fatalf("cjk = %d, latin = %d（期望约 4 倍）", cjk, latin)
	}
}

// TestEstimateInputTokensScalesLinearly 内容翻倍，估算也应约翻倍。
func TestEstimateInputTokensScalesLinearly(t *testing.T) {
	small := estimateInputTokens(ReserveInput{StringFields: []string{strings.Repeat("a", 1000)}})
	big := estimateInputTokens(ReserveInput{StringFields: []string{strings.Repeat("a", 2000)}})
	if big < small*3/2 || big > small*5/2 {
		t.Fatalf("small = %d, big = %d（应约 2 倍）", small, big)
	}
}

// TestEstimateInputTokensNeverEmpty 空输入至少按 1 token 预留，避免空预留。
func TestEstimateInputTokensNeverEmpty(t *testing.T) {
	if got := estimateInputTokens(ReserveInput{}); got != 1 {
		t.Fatalf("empty estimate = %d, want 1", got)
	}
}

// TestEstimateInputTokensPrefersPerMessageStrings 调用方同时给整段 JSON 与
// 每条消息 JSON 时必须只计一份：估算不随 InputText 的重复而翻倍。
func TestEstimateInputTokensPrefersPerMessageStrings(t *testing.T) {
	msg := `[{"role":"user","content":"` + strings.Repeat("测", 2000) + `"}]`
	withBoth := estimateInputTokens(ReserveInput{InputText: msg, StringFields: []string{msg, msg}})
	twoMessages := estimateInputTokens(ReserveInput{StringFields: []string{msg, msg}})
	if withBoth != twoMessages {
		t.Fatalf("withBoth = %d, twoMessages = %d（两份 StringFields 才应翻倍）", withBoth, twoMessages)
	}
}
