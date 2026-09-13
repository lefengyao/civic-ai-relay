package relay

import (
	"testing"

	"civic-ai-relay/internal/store"
	"civic-ai-relay/internal/upstream"
)

func usage(prompt, cached, completion int) upstream.Usage {
	var u upstream.Usage
	u.PromptTokens = prompt
	u.CompletionTokens = completion
	u.TotalTokens = prompt + completion
	u.PromptTokensDetails.CachedTokens = cached
	return u
}

func iptr(v int64) *int64 { return &v }

// 未配置缓存价：缓存命中的部分按普通输入价计费（与历史行为一致）。
func TestPriceUsageWithoutCachedPrice(t *testing.T) {
	model := store.Model{InputPriceMicroyuan: iptr(1_000_000), OutputPriceMicroyuan: iptr(2_000_000)}
	// 60 万普通输入 + 40 万缓存输入（回退输入价）+ 100 万输出
	got := PriceUsage(usage(1_000_000, 400_000, 1_000_000), model)
	if got != 3_000_000 {
		t.Fatalf("amount = %d, want 3_000_000", got)
	}
}

// 配置了缓存价：缓存部分按缓存价，其余部分按普通输入/输出价。
func TestPriceUsageWithCachedPrice(t *testing.T) {
	model := store.Model{InputPriceMicroyuan: iptr(1_000_000), OutputPriceMicroyuan: iptr(2_000_000), CachedInputPriceMicroyuan: iptr(500_000)}
	got := PriceUsage(usage(1_000_000, 400_000, 1_000_000), model)
	if got != 2_800_000 {
		t.Fatalf("amount = %d, want 2_800_000 (600k*1.0 + 400k*0.5 + 1M*2.0)", got)
	}
}

// 缓存价填 0 表示缓存命中免费。
func TestPriceUsageFreeCache(t *testing.T) {
	model := store.Model{InputPriceMicroyuan: iptr(1_000_000), CachedInputPriceMicroyuan: iptr(0)}
	if got := PriceUsage(usage(1_000_000, 400_000, 0), model); got != 600_000 {
		t.Fatalf("amount = %d, want 600_000", got)
	}
}

// 上游返回的 cached_tokens 超过 prompt_tokens 时钳制到 prompt_tokens。
func TestPriceUsageClampsCachedAbovePrompt(t *testing.T) {
	model := store.Model{InputPriceMicroyuan: iptr(1_000_000), CachedInputPriceMicroyuan: iptr(500_000)}
	// 100 tokens 全部按缓存价：100 * 500_000 / 1e6 = 50
	if got := PriceUsage(usage(100, 500, 0), model); got != 50 {
		t.Fatalf("amount = %d, want 50", got)
	}
}

// 未定价模型始终计 0。
func TestPriceUsageUnpricedModel(t *testing.T) {
	if got := PriceUsage(usage(1_000_000, 400_000, 1_000_000), store.Model{}); got != 0 {
		t.Fatalf("amount = %d, want 0", got)
	}
}

// 组倍率同样作用于缓存价。
func TestApplyRateCoversCachedPrice(t *testing.T) {
	model := store.Model{InputPriceMicroyuan: iptr(1_000_000), CachedInputPriceMicroyuan: iptr(500_000)}
	model.InputPriceMicroyuan = applyRate(model.InputPriceMicroyuan, 1500)
	model.CachedInputPriceMicroyuan = applyRate(model.CachedInputPriceMicroyuan, 1500)
	if *model.InputPriceMicroyuan != 1_500_000 || *model.CachedInputPriceMicroyuan != 750_000 {
		t.Fatalf("rate not applied: %v %v", *model.InputPriceMicroyuan, *model.CachedInputPriceMicroyuan)
	}
}
