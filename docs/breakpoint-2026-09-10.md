# 工作断点记录 — 2026-09-10 ✅ 已完成

> 「渠道多模型拉取 + 分组倍率」功能已全部开发、编译并在 Windows 端实测通过。本文档留作功能说明与验证记录。

## 功能总览（全部已上线）

| 能力 | 状态 | 说明 |
|---|---|---|
| 渠道多模型 | ✅ | 一个供应商（渠道）下挂多个模型 |
| 上游模型拉取 | ✅ | 供应商页「模型管理」弹窗 → 拉取上游 `/v1/models` → 勾选导入，**每个模型可自定义对外名称（映射）**；已导入的置灰标记（2026-09-11 由独立「拉取模型」按钮升级为统一弹窗，同时支持手动添加+定价+映射选择） |
| 手动添加模型 | ✅ | 模型页表单 |
| 单模型计价 | ✅ | 元/百万 token，输入/输出分别计价 |
| 分组倍率 | ✅ | `rate_milli` 千分比（1000=1.0×），创建/编辑分组可设置 |
| 根路径直达 | ✅ | `/` 302 到 `/admin/`，favicon 204 |
| 密钥标识+重置 | ✅ | Key 列表显示非敏感标识 `key-xxxxxx` + 脱敏缩略 `crk_xxxx…`（独立接口 `/keys/hints`）；「重置密钥」签发新密钥。迁移 v4/v5/v6。**安全不变量：密钥任何字符不得出现在列表接口**（admin_test 强制） |
| 删除能力 | ✅ | 四类资源均支持 `DELETE`：供应商（级联删其模型）、模型、模型组、Key（立即吊销）。历史账单金额一律保留（requests 外键 SET NULL）。重复删除返回 404 |
| Key 消耗用量 | ✅ | keysList 返回 UsedTokens/UsedAmountMicroyuan/RequestCount（口径与限额检查一致：含未结算预留）；UI「已用/限额」+ 进度条（≥90% 红色）+ 请求次数 |
| Key 更新 API | ✅ | `PUT keys/{id}` 统一 snake_case：`name/enabled/concurrency_limit/token_limit_total/money_limit_yuan_total`，nil 字段保持原值（修复了原先 snake_case 静默不生效的 bug） |

## 关键设计

- **倍率语义**：key 请求模型时，取其所属**启用**分组中包含该模型的**最大**倍率；无匹配回落 1000。原始模型单价存库不改写，只在预留与结算时折算（`relay.Begin` → `applyRate`）。
- **导入语义**：导入的模型为**停用 + 未定价**，需到「模型」页定价并启用后才能加入分组。
- **URL 归一化**：供应商 base_url 存库时去掉 `/v1` 后缀，upstream client 请求时自动补 `/v1`（`upstream/client.go:63`）。
- 迁移 v3：`model_groups.rate_milli INTEGER NOT NULL DEFAULT 1000 CHECK (>0)`。

## 验证记录（2026-09-10 19:0x，全部通过）

1. 迁移 v3 启动时自动应用（schema_migrations: 1,2,3）
2. 分组倍率 API：`PUT /admin/api/groups/1 {rate:1.5}` → RateMilli 1500
3. 拉取预览（假上游 `test-data/fake-upstream.py`，端口 8899，5 个模型）→ exists 标记正确
4. 批量导入含重复项 → 幂等，count=2，导入为停用未定价
5. **端到端计费**：模型 1元/M token × 输入输出各 100万 × 1.5倍率 = 扣费 3,000,000 microyuan（3元）✅ 精确匹配
6. 浏览器端到端：拉取弹窗勾选、分组倍率编辑 ×1.50→×2.00 保存刷新、截图 ui-10 ~ ui-14

## 测试数据残留（当前库里）

- 供应商：`测试上游`（api.example.com，不可用）、`本地测试渠道`（指向本地假上游）
- 模型：`test/demo-model`、`本地测试渠道/gpt-4o-mini`（1元/M，启用）、`本地测试渠道/deepseek-chat`（停用未定价）
- 分组：`默认组`（倍率 1.5×）
- Key：`测试客户端`、`倍率验证Key`（token 未留存，可在 UI 重新创建）
- 假上游脚本：`test-data/fake-upstream.py`（`python test-data/fake-upstream.py` 启动，端口 8899）

如需清空测试数据，可在 UI 逐个删除或直接删 `data/relay.db` 重建。
