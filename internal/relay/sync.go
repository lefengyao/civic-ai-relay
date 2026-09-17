package relay

import (
	"context"

	"civic-ai-relay/internal/logging"
	"civic-ai-relay/internal/store"
)

// CatalogSync 是自动同步所需的唯一上游能力：把某渠道上游的模型目录导入本地。
type CatalogSync interface {
	SyncProvider(ctx context.Context, providerID int64) ([]string, error)
}

// SyncSummary 汇总一轮同步的结果，用于日志。
type SyncSummary struct {
	Providers int // 检查过的启用渠道数
	Imported  int // 新导入的模型数
	Failed    int // 拉取失败的渠道数
}

// ModelSyncer 按 MODEL_SYNC_INTERVAL 把上游新增的模型导入本地。
//
// 语义上是「发现新模型」，不是「自动上线模型」：导入进来的模型一律是
// **停用 + 未定价**（沿用管理台「拉取上游模型」的同一约定，见 Registry.SyncProvider），
// 需要运维定价并启用后才可能被授权给客户端。这样自动同步不会绕过定价环节，
// 也不会因为上游多出一个模型就悄悄改变对外可用的模型列表。
type ModelSyncer struct {
	store   *store.Store
	catalog CatalogSync
}

func NewModelSyncer(repo *store.Store, catalog CatalogSync) *ModelSyncer {
	return &ModelSyncer{store: repo, catalog: catalog}
}

// RunOnce 遍历所有启用渠道各同步一次。单个渠道失败只记日志、不中断整轮，
// 否则一个坏渠道会让后面所有渠道都同步不上。
func (s *ModelSyncer) RunOnce(ctx context.Context) (SyncSummary, error) {
	summary := SyncSummary{}
	if s == nil || s.catalog == nil {
		return summary, nil
	}
	providers, err := s.store.ListProviders(ctx)
	if err != nil {
		return summary, err
	}
	for _, provider := range providers {
		if !provider.Enabled {
			continue
		}
		summary.Providers++
		imported, err := s.catalog.SyncProvider(ctx, provider.ID)
		if err != nil {
			summary.Failed++
			logging.Warnf("model sync: 渠道「%s」拉取上游模型失败: %v", provider.Name, err)
			continue
		}
		if len(imported) > 0 {
			logging.Infof("model sync: 渠道「%s」新增 %d 个模型（已导入为停用 + 未定价，需在管理台定价并启用）", provider.Name, len(imported))
			summary.Imported += len(imported)
		}
	}
	return summary, nil
}
