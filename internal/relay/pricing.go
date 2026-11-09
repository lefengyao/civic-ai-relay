package relay

import "civic-ai-relay/internal/store"

type PriceSnapshot struct {
	ModelID                   int64
	InputPriceMicroyuan       int64
	OutputPriceMicroyuan      int64
	CachedInputPriceMicroyuan int64
	RateMilli                 int64
}

func priceSnapshot(reservation store.RequestReservation) PriceSnapshot {
	return PriceSnapshot{
		ModelID:                   reservation.ModelID,
		InputPriceMicroyuan:       reservation.QuoteInputPriceMicroyuan,
		OutputPriceMicroyuan:      reservation.QuoteOutputPriceMicroyuan,
		CachedInputPriceMicroyuan: reservation.QuoteCachedInputPriceMicroyuan,
		RateMilli:                 reservation.QuoteRateMilli,
	}
}

func (p PriceSnapshot) effective(model store.Model) store.Model {
	input := applyRate(&p.InputPriceMicroyuan, p.RateMilli)
	output := applyRate(&p.OutputPriceMicroyuan, p.RateMilli)
	cached := applyRate(&p.CachedInputPriceMicroyuan, p.RateMilli)
	model.InputPriceMicroyuan = input
	model.OutputPriceMicroyuan = output
	model.CachedInputPriceMicroyuan = cached
	return model
}

// effectivePoolModel 按模型自己的单价折算分组倍率。故障转移供应商的成员各自
// 定价，切换后必须按实际成功成员的价格结算，而不是预留时首选成员的快照。
func effectivePoolModel(model store.Model, rateMilli int64) store.Model {
	model.InputPriceMicroyuan = applyRate(model.InputPriceMicroyuan, rateMilli)
	model.OutputPriceMicroyuan = applyRate(model.OutputPriceMicroyuan, rateMilli)
	model.CachedInputPriceMicroyuan = applyRate(model.CachedInputPriceMicroyuan, rateMilli)
	return model
}
