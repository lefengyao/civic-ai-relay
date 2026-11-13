package store

import (
	"context"
	"fmt"
)

type migration struct {
	version    int
	statements []string
}

var migrations = []migration{
	{version: 1, statements: []string{
		`CREATE TABLE providers (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			base_url TEXT NOT NULL,
			api_key_ciphertext BLOB NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
			created_at_utc TEXT NOT NULL,
			updated_at_utc TEXT NOT NULL
		)`,
		`CREATE TABLE models (
			id INTEGER PRIMARY KEY,
			provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			public_name TEXT NOT NULL UNIQUE,
			upstream_name TEXT NOT NULL,
			input_price_microyuan INTEGER,
			output_price_microyuan INTEGER,
			enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
			created_at_utc TEXT NOT NULL,
			updated_at_utc TEXT NOT NULL,
			CHECK (input_price_microyuan IS NULL OR input_price_microyuan >= 0),
			CHECK (output_price_microyuan IS NULL OR output_price_microyuan >= 0)
		)`,
		`CREATE TABLE model_groups (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
			created_at_utc TEXT NOT NULL,
			updated_at_utc TEXT NOT NULL
		)`,
		`CREATE TABLE group_models (
			group_id INTEGER NOT NULL REFERENCES model_groups(id) ON DELETE CASCADE,
			model_id INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
			PRIMARY KEY (group_id, model_id)
		)`,
		`CREATE TABLE client_keys (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			token_digest BLOB NOT NULL UNIQUE,
			enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
			concurrency_limit INTEGER NOT NULL CHECK (concurrency_limit > 0),
			token_limit INTEGER,
			amount_limit_microyuan INTEGER,
			disabled_reason TEXT,
			created_at_utc TEXT NOT NULL,
			updated_at_utc TEXT NOT NULL,
			CHECK (token_limit IS NULL OR token_limit >= 0),
			CHECK (amount_limit_microyuan IS NULL OR amount_limit_microyuan >= 0)
		)`,
		`CREATE TABLE key_groups (
			key_id INTEGER NOT NULL REFERENCES client_keys(id) ON DELETE CASCADE,
			group_id INTEGER NOT NULL REFERENCES model_groups(id) ON DELETE CASCADE,
			PRIMARY KEY (key_id, group_id)
		)`,
		`CREATE TABLE key_reservations (
			id INTEGER PRIMARY KEY,
			key_id INTEGER NOT NULL REFERENCES client_keys(id) ON DELETE CASCADE,
			model_id INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
			reserved_tokens INTEGER NOT NULL CHECK (reserved_tokens >= 0),
			reserved_amount_microyuan INTEGER NOT NULL CHECK (reserved_amount_microyuan >= 0),
			charged_tokens INTEGER NOT NULL DEFAULT 0 CHECK (charged_tokens >= 0),
			charged_amount_microyuan INTEGER NOT NULL DEFAULT 0 CHECK (charged_amount_microyuan >= 0),
			status TEXT NOT NULL CHECK (status IN ('reserved', 'completed', 'failed', 'aborted', 'rejected')),
			created_at_utc TEXT NOT NULL,
			finished_at_utc TEXT
		)`,
		`CREATE TABLE requests (
			id INTEGER PRIMARY KEY,
			request_id TEXT NOT NULL UNIQUE,
			key_id INTEGER REFERENCES client_keys(id) ON DELETE SET NULL,
			model_id INTEGER REFERENCES models(id) ON DELETE SET NULL,
			provider_id INTEGER REFERENCES providers(id) ON DELETE SET NULL,
			status TEXT NOT NULL,
			input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
			output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
			amount_microyuan INTEGER NOT NULL DEFAULT 0 CHECK (amount_microyuan >= 0),
			upstream_status INTEGER,
			streamed INTEGER NOT NULL DEFAULT 0 CHECK (streamed IN (0, 1)),
			created_at_utc TEXT NOT NULL,
			finished_at_utc TEXT
		)`,
		`CREATE INDEX idx_client_keys_enabled ON client_keys(enabled, id)`,
		`CREATE INDEX idx_models_provider_enabled ON models(provider_id, enabled, id)`,
		`CREATE INDEX idx_group_models_model ON group_models(model_id, group_id)`,
		`CREATE INDEX idx_key_groups_group ON key_groups(group_id, key_id)`,
		`CREATE INDEX idx_key_reservations_key_status_time ON key_reservations(key_id, status, created_at_utc)`,
		`CREATE INDEX idx_key_reservations_time ON key_reservations(created_at_utc, status)`,
		`CREATE INDEX idx_requests_time ON requests(created_at_utc)`,
		`CREATE INDEX idx_requests_key_time ON requests(key_id, created_at_utc)`,
	}},
	{version: 2, statements: []string{
		`ALTER TABLE requests ADD COLUMN billing_date_bj TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requests ADD COLUMN reserved_tokens INTEGER NOT NULL DEFAULT 0 CHECK (reserved_tokens >= 0)`,
		`ALTER TABLE requests ADD COLUMN charged_tokens INTEGER NOT NULL DEFAULT 0 CHECK (charged_tokens >= 0)`,
		`ALTER TABLE key_reservations ADD COLUMN request_id TEXT`,
		`ALTER TABLE key_reservations ADD COLUMN billing_date_bj TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX idx_key_reservations_request ON key_reservations(request_id)`,
		`CREATE INDEX idx_requests_billing_date ON requests(billing_date_bj, created_at_utc)`,
	}},
	{version: 3, statements: []string{
		`ALTER TABLE model_groups ADD COLUMN rate_milli INTEGER NOT NULL DEFAULT 1000 CHECK (rate_milli > 0)`,
	}},
	{version: 4, statements: []string{
		`ALTER TABLE client_keys ADD COLUMN token_label TEXT NOT NULL DEFAULT ''`,
	}},
	{version: 5, statements: []string{
		`UPDATE client_keys SET token_label = 'key-' || lower(hex(randomblob(4))) WHERE token_label = ''`,
	}},
	{version: 6, statements: []string{
		`ALTER TABLE client_keys ADD COLUMN token_hint TEXT NOT NULL DEFAULT ''`,
	}},
	{version: 7, statements: []string{
		`ALTER TABLE models ADD COLUMN cached_input_price_microyuan INTEGER CHECK (cached_input_price_microyuan IS NULL OR cached_input_price_microyuan >= 0)`,
		`ALTER TABLE requests ADD COLUMN cached_input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cached_input_tokens >= 0)`,
	}},
	// v8: 模型组从"管理模型"改为"管理供应商（渠道）"。组内挂渠道，授权模型 =
	// 组内启用渠道下的所有已定价启用模型。历史组内模型成员折算为其所属渠道。
	{version: 8, statements: []string{
		`CREATE TABLE group_providers (
			group_id INTEGER NOT NULL REFERENCES model_groups(id) ON DELETE CASCADE,
			provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			PRIMARY KEY (group_id, provider_id)
		)`,
		`INSERT INTO group_providers(group_id, provider_id)
			SELECT DISTINCT gm.group_id, m.provider_id FROM group_models gm JOIN models m ON m.id = gm.model_id`,
		`DROP INDEX IF EXISTS idx_group_models_model`,
		`DROP TABLE group_models`,
		`CREATE INDEX idx_group_providers_provider ON group_providers(provider_id, group_id)`,
	}},
	// v9: 取消「Key 总配额用满即自动停用」。旧代码会把 Key 置为 enabled=0 +
	// disabled_reason='quota_exhausted'，而调大限额后它不会自动恢复——运维看到
	// 的是"额度明明还有，请求却说无法使用"，还得手工把 Key 启回来。额度判定已
	// 改到每次请求时进行（欠费即拒，限额调大或窗口轮换后自动恢复），所以这里把
	// 历史遗留的自动停用恢复成启用。手动停用（disabled_reason 为其它值）不动。
	{version: 9, statements: []string{
		`UPDATE client_keys SET enabled=1, disabled_reason='' WHERE disabled_reason='quota_exhausted'`,
	}},
	// v10: 分组可用性监测结果。每轮监测每个分组写一行（不是每个渠道一行——
	// detail 里已经写清哪些渠道失败），组数×轮次才是行数增长量，很有限。
	// 说明：本功能的定位是「发现并展示」，不改变服务行为，所以没有自动停用渠道
	// 之类的副作用字段。
	{version: 10, statements: []string{
		`CREATE TABLE group_monitor_results (
			id INTEGER PRIMARY KEY,
			group_id INTEGER NOT NULL REFERENCES model_groups(id) ON DELETE CASCADE,
			status TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '',
			total_providers INTEGER NOT NULL DEFAULT 0,
			failed_providers INTEGER NOT NULL DEFAULT 0,
			latency_ms INTEGER NOT NULL DEFAULT 0,
			trigger TEXT NOT NULL DEFAULT 'auto',
			created_at_utc TEXT NOT NULL
		)`,
		`CREATE INDEX idx_group_monitor_latest ON group_monitor_results(group_id, id DESC)`,
		`CREATE INDEX idx_group_monitor_time ON group_monitor_results(created_at_utc)`,
	}},
	// v11: 客户端 Key 支持有效期（短时令牌）。expires_at_utc 为 NULL = 永久有效，
	// 与升级前的行为完全一致，所以历史 Key 不会因为这次迁移突然失效——「必须有
	// 效期」是运维的显式选择，不该由迁移代劳。判定在认证路径上做（AuthenticateClientKey），
	// 过期返回 401 token_expired，与 invalid_api_key（无效/停用）区分开，客户端据此
	// 能判断是「该换新令牌了」还是「这个令牌根本不对」。
	{version: 11, statements: []string{
		`ALTER TABLE client_keys ADD COLUMN expires_at_utc TEXT`,
		`CREATE INDEX idx_client_keys_expiry ON client_keys(expires_at_utc)`,
	}},
	// v12: 应用型凭据 ——「客户端里不存在密钥」的接入方式。
	// public_id 非空表示该记录用请求头 X-App-Id 接入，且 **没有可用的密钥**：
	// token_digest 里是创建时即丢弃的随机值，没有任何人知道它的明文。客户端里没有
	// 可提取的东西，用户也就无法把凭据带到应用外去用。
	// ⚠️ 代价必须说清楚：应用标识是公开的（就写在客户端里），知道它的人都能调用，
	// 所以真正的安全边界落在「谁能访问到这个网关」上（内网、防火墙、反代鉴权）。
	{version: 12, statements: []string{
		`ALTER TABLE client_keys ADD COLUMN public_id TEXT`,
		`CREATE UNIQUE INDEX idx_client_keys_public_id ON client_keys(public_id) WHERE public_id IS NOT NULL`,
	}},
	{version: 13, statements: []string{
		`ALTER TABLE requests ADD COLUMN error_code TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requests ADD COLUMN failure_stage TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requests ADD COLUMN usage_source TEXT NOT NULL DEFAULT 'unknown'`,
		`ALTER TABLE requests ADD COLUMN protocol TEXT NOT NULL DEFAULT 'chat'`,
		`ALTER TABLE requests ADD COLUMN client_request_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requests ADD COLUMN incomplete_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requests ADD COLUMN duration_ms INTEGER`,
		`ALTER TABLE requests ADD COLUMN upstream_header_ms INTEGER`,
		`ALTER TABLE requests ADD COLUMN first_event_ms INTEGER`,
		`ALTER TABLE requests ADD COLUMN last_upstream_ms INTEGER`,
		`ALTER TABLE requests ADD COLUMN quote_input_microyuan INTEGER`,
		`ALTER TABLE requests ADD COLUMN quote_output_microyuan INTEGER`,
		`ALTER TABLE requests ADD COLUMN quote_cached_microyuan INTEGER`,
		`ALTER TABLE requests ADD COLUMN quote_rate_milli INTEGER`,
		`CREATE TRIGGER prevent_active_reservation_delete BEFORE DELETE ON key_reservations
			WHEN OLD.status='reserved'
			BEGIN SELECT RAISE(ABORT, 'active reservation must be settled before deletion'); END`,
		`CREATE INDEX idx_requests_status_id ON requests(status, id)`,
		`CREATE INDEX idx_requests_provider_id ON requests(provider_id, id)`,
		`CREATE INDEX idx_requests_model_id ON requests(model_id, id)`,
		`CREATE INDEX idx_requests_protocol_id ON requests(protocol, id)`,
		`CREATE INDEX idx_requests_created_id ON requests(created_at_utc, id)`,
	}},
	{version: 14, statements: []string{
		`CREATE TABLE civic_plugins (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			version TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			package_path TEXT NOT NULL UNIQUE,
			wasm_sha256 TEXT NOT NULL,
			manifest_json TEXT NOT NULL,
			permissions_json TEXT NOT NULL DEFAULT '[]',
			limits_json TEXT NOT NULL DEFAULT '{}',
			enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
			status TEXT NOT NULL DEFAULT 'installed',
			last_error TEXT NOT NULL DEFAULT '',
			installed_at_utc TEXT NOT NULL,
			updated_at_utc TEXT NOT NULL,
			last_started_at_utc TEXT,
			last_stopped_at_utc TEXT,
			start_count INTEGER NOT NULL DEFAULT 0 CHECK (start_count >= 0),
			failure_count INTEGER NOT NULL DEFAULT 0 CHECK (failure_count >= 0)
		)`,
		`CREATE INDEX idx_civic_plugins_enabled ON civic_plugins(enabled, id)`,
	}},
	{version: 15, statements: []string{
		`ALTER TABLE providers ADD COLUMN protocol TEXT NOT NULL DEFAULT 'openai'`,
		`CREATE INDEX idx_providers_protocol_enabled ON providers(protocol, enabled, id)`,
	}},
	// v16: 密钥与分组改为一对一（设计见 docs/superpowers/specs/2026-11-01-civic-relay-
	// group-scoping-design.md 第五节）。client_keys.group_id 取代 key_groups
	// 多对多表；两张账目表冗余一份 group_id，记录请求发生时的归属 —— 否则
	// 「切换分组」会把历史用量整体搬到新组：旧组已用凭空减少、新组凭空增加，
	// 额度判定被篡改。
	//
	// 回填规则取「倍率最大的组，同倍率取 id 最小」：这正是旧 MAX(g.rate_milli)
	// 的计价口径，保证曾经多组的密钥迁移后计费倍率不变。
	// 边界：回填不看组的 enabled 与渠道授权——最高倍率组恰好停用的密钥会被绑到
	// 停用组，升级后该密钥无可用模型，需要运维改绑（接入信息里能看到 issue）。
	//
	// key_groups 此时**保留**，由 v17 删除 —— 本条回填要以它为数据源。
	// 对曾经属于多个组的密钥，账目表的历史行只能按迁移后的单一归属回填，是近似值。
	{version: 16, statements: []string{
		`ALTER TABLE client_keys ADD COLUMN group_id INTEGER REFERENCES model_groups(id) ON DELETE SET NULL`,
		`UPDATE client_keys SET group_id = (
			SELECT kg.group_id FROM key_groups kg JOIN model_groups g ON g.id = kg.group_id
			WHERE kg.key_id = client_keys.id
			ORDER BY g.rate_milli DESC, g.id ASC LIMIT 1
		)`,
		`ALTER TABLE key_reservations ADD COLUMN group_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN group_id INTEGER NOT NULL DEFAULT 0`,
		`UPDATE key_reservations SET group_id = COALESCE((SELECT group_id FROM client_keys WHERE id = key_reservations.key_id), 0)`,
		`UPDATE requests SET group_id = COALESCE((SELECT group_id FROM client_keys WHERE id = requests.key_id), 0)`,
	}},
	// v17: key_groups 的历史使命结束（v16 已按它回填 client_keys.group_id），
	// 删表并建分组维度索引。额度窗口查询在每次请求的事务里按 group_id + 时间
	// 过滤，没这两个索引就是每次请求全表扫。
	{version: 17, statements: []string{
		`DROP INDEX IF EXISTS idx_key_groups_group`,
		`DROP TABLE key_groups`,
		`CREATE INDEX idx_key_reservations_group_time ON key_reservations(group_id, created_at_utc)`,
		`CREATE INDEX idx_requests_group_time ON requests(group_id, created_at_utc)`,
	}},
	// v18: 分组额度（取代全局额度，设计见 docs/superpowers/specs/2026-11-01-…
	// -group-scoping-design.md 第 5.1/5.3 节）。10 列全部可空：NULL 或 0 = 不限，
	// 沿用「限额是运维显式选择」的既有口径——升级后所有分组默认不限，不会突然
	// 出现意外拒绝。
	//
	// 全局额度值在 relay.env（配置）而不是数据库，所以「搬进每个现有分组」
	// 不能用 SQL 迁移完成：由启动时的播种逻辑读原值 → 只填 IS NULL 的列 →
	// 从 relay.env 删键（删键本身就是幂等标记，见 cmd/civic-relay/main.go）。
	{version: 18, statements: []string{
		`ALTER TABLE model_groups ADD COLUMN token_limit_5h INTEGER`,
		`ALTER TABLE model_groups ADD COLUMN amount_limit_5h INTEGER`,
		`ALTER TABLE model_groups ADD COLUMN token_limit_daily INTEGER`,
		`ALTER TABLE model_groups ADD COLUMN amount_limit_daily INTEGER`,
		`ALTER TABLE model_groups ADD COLUMN token_limit_weekly INTEGER`,
		`ALTER TABLE model_groups ADD COLUMN amount_limit_weekly INTEGER`,
		`ALTER TABLE model_groups ADD COLUMN period_limit_days INTEGER`,
		`ALTER TABLE model_groups ADD COLUMN token_limit_period INTEGER`,
		`ALTER TABLE model_groups ADD COLUMN amount_limit_period INTEGER`,
		`ALTER TABLE model_groups ADD COLUMN period_limit_start TEXT`,
	}},
	// v19: 故障转移供应商（模型池）。与 providers 完全分开的第二种「供应商形式」：
	// 一个池只对应一个对外模型名（单一模型类别），成员是**已有供应商下的模型**，
	// 按 sort_order 排优先级；成员故障时按顺序自动切到下一个可用模型。
	//
	// 纯新增两张表，不改任何既有表：升级不影响现有渠道、模型、分组、账目。
	// 成员随模型/池删除级联消失，历史账目仍指向真实模型 ID（不受池删除影响）。
	{version: 19, statements: []string{
		`CREATE TABLE model_pools (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
			created_at_utc TEXT NOT NULL,
			updated_at_utc TEXT NOT NULL
		)`,
		`CREATE TABLE model_pool_members (
			pool_id INTEGER NOT NULL REFERENCES model_pools(id) ON DELETE CASCADE,
			model_id INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
			sort_order INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
			PRIMARY KEY (pool_id, model_id)
		)`,
		`CREATE INDEX idx_model_pool_members_order ON model_pool_members(pool_id, sort_order, model_id)`,
		`CREATE INDEX idx_model_pool_members_model ON model_pool_members(model_id, pool_id)`,
	}},
	// v20: 账本独立于主数据。旧表的 key_id/model_id 带 ON DELETE CASCADE，
	// 删一把 Key（或删模型、删渠道）会在级联里把整段额度账目一起删掉——而额度是
	// 按 group_id 统计的，于是「删一次 Key」就等于把该组的周期/总额度用量清零，
	// 表现为「额度用满后又从 0 开始计算」。账本行只是计费数字，不含 prompt/密钥/回复，
	// 没有任何理由跟随主数据消失。
	//
	// 重建为不带外键的独立表：key_id / model_id 变成纯归属标记（0 = 对应对象已删除），
	// group_id 不变，统计口径不受影响。最后按 requests 的实扣值补回旧版本已经丢掉的
	// 账目（只在缺行时插入，可重复执行）。
	{version: 20, statements: []string{
		`CREATE TABLE key_reservations_v20 (
			id INTEGER PRIMARY KEY,
			key_id INTEGER NOT NULL DEFAULT 0,
			model_id INTEGER NOT NULL DEFAULT 0,
			reserved_tokens INTEGER NOT NULL CHECK (reserved_tokens >= 0),
			reserved_amount_microyuan INTEGER NOT NULL CHECK (reserved_amount_microyuan >= 0),
			charged_tokens INTEGER NOT NULL DEFAULT 0 CHECK (charged_tokens >= 0),
			charged_amount_microyuan INTEGER NOT NULL DEFAULT 0 CHECK (charged_amount_microyuan >= 0),
			status TEXT NOT NULL CHECK (status IN ('reserved', 'completed', 'failed', 'aborted', 'rejected')),
			created_at_utc TEXT NOT NULL,
			finished_at_utc TEXT,
			request_id TEXT,
			billing_date_bj TEXT NOT NULL DEFAULT '',
			group_id INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT INTO key_reservations_v20 (id,key_id,model_id,reserved_tokens,reserved_amount_microyuan,charged_tokens,charged_amount_microyuan,status,created_at_utc,finished_at_utc,request_id,billing_date_bj,group_id)
			SELECT id,key_id,model_id,reserved_tokens,reserved_amount_microyuan,charged_tokens,charged_amount_microyuan,status,created_at_utc,finished_at_utc,request_id,billing_date_bj,group_id FROM key_reservations`,
		`DROP TABLE key_reservations`,
		`ALTER TABLE key_reservations_v20 RENAME TO key_reservations`,
		`CREATE INDEX idx_key_reservations_key_status_time ON key_reservations(key_id, status, created_at_utc)`,
		`CREATE INDEX idx_key_reservations_time ON key_reservations(created_at_utc, status)`,
		`CREATE INDEX idx_key_reservations_request ON key_reservations(request_id)`,
		`CREATE INDEX idx_key_reservations_group_time ON key_reservations(group_id, created_at_utc)`,
		`CREATE TRIGGER prevent_active_reservation_delete BEFORE DELETE ON key_reservations
			WHEN OLD.status='reserved'
			BEGIN SELECT RAISE(ABORT, 'active reservation must be settled before deletion'); END`,
		`INSERT INTO key_reservations (key_id,model_id,reserved_tokens,reserved_amount_microyuan,charged_tokens,charged_amount_microyuan,status,created_at_utc,finished_at_utc,request_id,billing_date_bj,group_id)
			SELECT COALESCE(r.key_id,0),COALESCE(r.model_id,0),0,0,r.charged_tokens,r.amount_microyuan,r.status,r.created_at_utc,r.finished_at_utc,r.request_id,COALESCE(r.billing_date_bj,''),COALESCE(r.group_id,0)
			FROM requests r
			WHERE r.request_id IS NOT NULL
				AND r.status IN ('completed','failed','aborted')
				AND NOT EXISTS (SELECT 1 FROM key_reservations k WHERE k.request_id=r.request_id)`,
	}},
	// v21: 供应商生效时段。渠道可以只在选定时间段启用：固定日期区间（临时按天采购）
	// 或每天重复时段（夜间便宜/夜间才可用），其余时间该渠道下的模型自动不可用。
	//
	// 纯新增列，默认 'always' = 全天不限，升级后所有既有渠道行为完全不变。
	// 时段判定在 Go 侧做（schedule_mode 决定看哪些列），SQL 只负责存原值：
	// 跨零点、星期选择这类规则用 SQL 表达又长又容易和时区打架。
	{version: 21, statements: []string{
		`ALTER TABLE providers ADD COLUMN schedule_mode TEXT NOT NULL DEFAULT 'always'`,
		`ALTER TABLE providers ADD COLUMN schedule_start_utc TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE providers ADD COLUMN schedule_end_utc TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE providers ADD COLUMN schedule_start_minute INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE providers ADD COLUMN schedule_end_minute INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE providers ADD COLUMN schedule_weekdays INTEGER NOT NULL DEFAULT 0`,
	}},
	// v22: 聚合模型组。一个组可以是 normal（自己挂渠道）或 aggregate（除自己的
	// 渠道外，还包含若干普通模型组，从而合并它们的渠道与模型）。
	//
	// 为什么复用 model_groups 而不是另建一张表：额度、倍率、Key 绑定、概览、
	// 账目归属全都以 model_groups.id 为主键，另建表等于把这些逻辑复制一份，
	// 两份实现早晚漂移。聚合组按自己的 rate_milli 与额度列计费，天然满足
	// 「聚合组拥有单独的计费」。
	//
	// 计费口径（老板明确要求「两个组都扣」）：一笔请求同时占用 Key 绑定组
	// （group_id）与真正提供该模型的成员组（source_group_id）的额度。账本仍
	// 只写一行 —— 两行会让所有 SUM 翻倍、结算要改多行，是计费系统最容易埋
	// 雷的地方。成员组的用量视图按 (group_id=? OR source_group_id=?) 统计，
	// 与额度判定同口径。
	//
	// 只允许一层：aggregate → normal。成员关系在应用层校验，避免环与递归。
	{version: 22, statements: []string{
		`ALTER TABLE model_groups ADD COLUMN kind TEXT NOT NULL DEFAULT 'normal'`,
		`CREATE TABLE group_compositions (
			parent_group_id INTEGER NOT NULL REFERENCES model_groups(id) ON DELETE CASCADE,
			child_group_id INTEGER NOT NULL REFERENCES model_groups(id) ON DELETE CASCADE,
			PRIMARY KEY (parent_group_id, child_group_id),
			CHECK (parent_group_id <> child_group_id)
		)`,
		`CREATE INDEX idx_group_compositions_child ON group_compositions(child_group_id, parent_group_id)`,
		`ALTER TABLE requests ADD COLUMN source_group_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE key_reservations ADD COLUMN source_group_id INTEGER NOT NULL DEFAULT 0`,
		`CREATE INDEX idx_requests_source_group_time ON requests(source_group_id, created_at_utc)`,
		`CREATE INDEX idx_key_reservations_source_group_time ON key_reservations(source_group_id, created_at_utc)`,
	}},
	// v23：把「只有聚合组能对外」这条规则落到既有数据上。
	//
	// v22 里两个类型都能被 Key 绑定，于是不存在「成员组 vs 可调用组」的区别；升级
	// 上来之后如果不动数据，所有既有组都会变成成员组——它们本来是对外的组，会突然
	// 一把 Key 都调不动。所以这里把**没有被任何聚合组包含**的 normal 组改成
	// aggregate：它们历史上就是独立对外的组，语义不变。
	//
	// 只有「已经是某个聚合组成员」的 normal 组保留成员身份。这种组如果历史上还绑着
	// Key（v22 允许），那把 Key 升级后确实调不动了——这是规则收紧的必然结果，管理台
	// 的 Key 列表会标出来，改绑到聚合组即可恢复。没有密钥的成员组不受任何影响。
	//
	// 结构上没有变更（kind 列 v22 就有），所以不需要 ALTER：这条 UPDATE 只是给旧行
	// 补上正确语义，新库执行时表里还没有任何组，等于空操作。
	{version: 23, statements: []string{
		`UPDATE model_groups SET kind='aggregate'
			WHERE kind='normal'
			  AND id NOT IN (SELECT child_group_id FROM group_compositions)`,
	}},
	// v24：一个渠道只能属于一个组。
	//
	// 之前的模型允许同一渠道挂进多个组，于是计费必须替它挑一个「真正提供者」：
	// 聚合组优先自有渠道，否则取成员组里 ID 最小的那个。结果是最小 ID 的组承担
	// 全部用量、其余组永远显示 0，而请求实际上跑的是它们的渠道；只有最小 ID 组
	// 的额度用满时才会拒绝请求，哪怕别的组还有大量余额。
	//
	// 这里把历史重复关系收敛成一条：**保留最早建立的那条归属**（rowid 最小），
	// 因为它是「谁先要的这个渠道」，也是计费一直在用的那句话。收敛之后建唯一
	// 索引，把规则钉在库上——应用层校验可能被绕过（脚本、手工 SQL），索引不会。
	{version: 24, statements: []string{
		`DELETE FROM group_providers WHERE rowid NOT IN (SELECT MIN(rowid) FROM group_providers GROUP BY provider_id)`,
		`DROP INDEX IF EXISTS idx_group_providers_provider`,
		`CREATE UNIQUE INDEX idx_group_providers_provider ON group_providers(provider_id)`,
	}},
	// v25：「每天重复」支持多段。
	//
	// 原来的 daily 只有一对起止分钟，表达不了「夜间 + 午休」这类一天里的多个窗口，
	// 运维只能拆成两条渠道（在归属唯一的规则下代价更大），或者干脆不配时段。
	//
	// 新列存 JSON 数组 `[{"StartMinute":1320,"EndMinute":480},…]`；判定在 Go 侧做
	// （ActiveAt），SQL 依旧只存原值。旧的三列继续写**第一段**，这样回滚到旧版本
	// 时渠道不会突然变成「全天可用」——只是少了几段，而不是时段保护整个失效。
	// 新列留空表示「没配多段」，读取时回落到旧列，v25 之前的数据不需要回填。
	{version: 25, statements: []string{
		`ALTER TABLE providers ADD COLUMN schedule_windows TEXT NOT NULL DEFAULT ''`,
	}},
}

func (s *Store) applySchema(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at_utc TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema migrations table: %w", err)
	}
	for _, migration := range migrations {
		var applied int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version = ?`, migration.version).Scan(&applied); err != nil {
			return fmt.Errorf("check schema migration %d: %w", migration.version, err)
		}
		if applied != 0 {
			continue
		}
		for _, statement := range migration.statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply schema migration %d: %w", migration.version, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_utc) VALUES (?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))`, migration.version); err != nil {
			return fmt.Errorf("record schema migration %d: %w", migration.version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	return nil
}
