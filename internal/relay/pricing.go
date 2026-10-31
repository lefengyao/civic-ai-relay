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
