# Civic Relay Multi-Tenant Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 Civic Relay 升级为支持多客户端 Key、一次性 Token/人民币额度、模型组授权、多供应商路由与 C 盘外置配置的局域网服务。

**Architecture:** 保留现有 FastAPI、单进程运行时和 SQLite 账本。新增 `tenant_store.py` 负责供应商、模型、分组和客户端 Key 的持久化与原子额度事务；新增 `provider_registry.py` 负责解密供应商密钥和按模型路由；`app.py` 只负责协议适配，把认证、授权、预留和结算委托给存储层。管理 API 与中文内嵌页面通过资源 ID 操作，原始客户端 Key 只在创建响应中出现一次。

**Tech Stack:** Python 3.11+, FastAPI, httpx, SQLite, `cryptography.fernet.Fernet`, pytest/pytest-asyncio。

---

### Task 1: 外置 C 盘配置路径和密钥加密基础

**Files:**
- Modify: `config_store.py`
- Modify: `config.py`
- Modify: `.env.example`
- Modify: `README.md`
- Modify: `pyproject.toml`
- Modify: `requirements.txt`
- Modify: `uv.lock`
- Create: `scripts/migrate_config.ps1`
- Modify: `tests/test_config_store.py`
- Modify: `tests/test_config.py`

- [ ] **Step 1: Write the failing tests**

在 `tests/test_config_store.py` 增加：

```python
def test_default_managed_config_path_is_outside_project(monkeypatch):
    monkeypatch.delenv("CIVIC_RELAY_CONFIG_FILE", raising=False)
    path = managed_config_path()
    assert "civicrelay" in str(path).lower()
    assert path.name == "relay.env"

def test_config_store_creates_parent_directory_before_atomic_write(tmp_path):
    path = tmp_path / "nested" / "relay.env"
    store = ConfigStore(path)
    store.write(Settings.from_mapping(test_values()))
    assert path.exists()
    assert path.parent.exists()
```

在 `tests/test_config.py` 增加：

```python
def test_relay_encryption_key_is_required_and_redacted(tmp_path):
    values = required_values(tmp_path)
    values["RELAY_ENCRYPTION_KEY"] = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
    settings = Settings.from_mapping(values)
    assert settings.relay_encryption_key == values["RELAY_ENCRYPTION_KEY"]
    assert "RELAY_ENCRYPTION_KEY" not in settings.redacted()

def test_settings_reject_missing_relay_encryption_key(tmp_path):
    with pytest.raises(SettingsError, match="RELAY_ENCRYPTION_KEY"):
        Settings.from_mapping(required_values(tmp_path))
```

- [ ] **Step 2: Run tests to verify they fail**

运行 `pytest tests/test_config_store.py::test_default_managed_config_path_is_outside_project tests/test_config_store.py::test_config_store_creates_parent_directory_before_atomic_write tests/test_config.py::test_relay_encryption_key_is_required_and_redacted -q`。预期失败，因为默认路径仍为 `.env`、父目录未创建且 `Settings` 没有加密密钥字段。

- [ ] **Step 3: Implement the minimal configuration changes**

在 `config_store.py` 中让 `managed_config_path()` 使用 `CIVIC_RELAY_CONFIG_FILE`，未设置时 Windows 返回 `Path(os.environ.get("PROGRAMDATA", r"C:\\ProgramData")) / "CivicRelay" / "relay.env"`，其他系统返回 `Path("/etc/civic-relay/relay.env")`；`write()` 先执行 `self.path.parent.mkdir(parents=True, exist_ok=True)`，再创建临时文件。

在 `config.py` 中新增 `relay_encryption_key: str` 字段，使用 `required(values, "RELAY_ENCRYPTION_KEY")` 读取，并把它加入 `SECRET_SETTING_NAMES`，但不加入 `redacted()` 的可见值。

在 `.env.example` 增加：

```dotenv
RELAY_ENCRYPTION_KEY=generate-a-fernet-key
```

在 `pyproject.toml` 和 `requirements.txt` 增加 `cryptography>=43,<47`，运行 `uv lock` 与 `uv export --format requirements-txt --no-hashes -o requirements.txt` 更新锁定/容器依赖。README 的本地启动命令改为先运行迁移脚本，再用 `--env-file C:\ProgramData\CivicRelay\relay.env` 启动。

创建 `scripts/migrate_config.ps1`，只在目标不存在时将项目目录的 `.env` 原样复制到 `C:\ProgramData\CivicRelay\relay.env`，补充随机 Fernet `RELAY_ENCRYPTION_KEY`，并限制 ACL。脚本在新路径解析和配置文件权限检查通过后才输出新路径；它绝不修改或删除原 `.env`。

- [ ] **Step 4: Run tests to verify they pass**

运行 `pytest tests/test_config_store.py tests/test_config.py -q`，预期全部通过。

- [ ] **Step 5: Commit**

当前目录不是 Git 仓库，跳过提交；保留文件变更供下一任务使用。

### Task 2: 多租户 SQLite schema 与安全值对象

**Files:**
- Create: `tenant_store.py`
- Modify: `db.py`
- Test: `tests/test_tenant_store.py`
- Modify: `tests/conftest.py`

- [ ] **Step 1: Write the failing tests**

创建 `tests/test_tenant_store.py`，先覆盖以下行为：

```python
async def test_create_key_returns_secret_once_and_list_is_redacted(tenant_store):
    created = await tenant_store.create_client_key("alice", 2, 1000, 3_000_000)
    assert created.token.startswith("crk_")
    rows = await tenant_store.list_client_keys()
    assert rows[0]["name"] == "alice"
    assert rows[0]["token"] is None
    assert created.token not in str(rows)

async def test_key_can_join_multiple_groups_and_models_are_filtered(tenant_store):
    basic = await tenant_store.create_model_group("基础组")
    advanced = await tenant_store.create_model_group("高级组")
    basic_model = await tenant_store.create_model("basic", "provider", "basic", 0, 0, True)
    advanced_model = await tenant_store.create_model("advanced", "provider", "advanced", 0, 0, True)
    await tenant_store.set_group_models(basic.id, [basic_model.id])
    await tenant_store.set_group_models(advanced.id, [advanced_model.id])
    key = await tenant_store.create_client_key("alice", 2, None, None)
    await tenant_store.set_key_groups(key.id, [basic.id, advanced.id])
    assert {m.public_name for m in await tenant_store.authorized_models(key.token)} == {"basic", "advanced"}

async def test_unpriced_model_cannot_be_added_to_group(tenant_store):
    group = await tenant_store.create_model_group("基础组")
    model = await tenant_store.create_model("new", "provider", "new", None, None, False)
    with pytest.raises(ValueError, match="model must be priced and enabled"):
        await tenant_store.set_group_models(group.id, [model.id])
```

- [ ] **Step 2: Run tests to verify they fail**

运行 `pytest tests/test_tenant_store.py -q`，预期失败，因为 `tenant_store.py` 尚不存在。

- [ ] **Step 3: Implement schema and key handling**

在 `tenant_store.py` 实现 `TenantStore`，复用 `Ledger` 的 SQLite 路径和线程锁。初始化时创建 `providers`、`models`、`model_groups`、`model_group_members`、`client_keys`、`client_key_groups` 表及索引；所有外键启用 `PRAGMA foreign_keys=ON`。

客户端 Key 格式固定为 `crk_<uuid4 hex>_<urlsafe random 32 bytes>`；只存 `key_prefix` 和 `sha256(token)` 摘要。实现 `create_client_key`、`authenticate_key`、`list_client_keys`、`set_key_groups`、`authorized_models`，列表响应永不包含原文。

模型价格以 `INTEGER` 微元/百万 Token 存储；`None` 表示未定价，0 表示免费。`set_group_models` 在事务内拒绝未启用或未定价模型。所有 DB 方法使用 `asyncio.to_thread` 调用同步实现。

在 `db.py` 的 `requests` 建表逻辑增加 `client_key_id`、`provider_id`、`reserved_money_microyuan`、`charged_money_microyuan` 列，并为 `client_key_id` 建索引；旧数据库用 `ALTER TABLE` 幂等补列。

- [ ] **Step 4: Run tests to verify they pass**

运行 `pytest tests/test_tenant_store.py tests/test_db.py -q`，预期通过且旧账本测试保持绿色。

- [ ] **Step 5: Commit**

跳过提交，继续同一工作区。

### Task 3: Key 级原子额度预留、结算与并发守卫

**Files:**
- Modify: `tenant_store.py`
- Modify: `limiter.py`
- Modify: `db.py`
- Test: `tests/test_tenant_store.py`
- Test: `tests/test_limiter.py`

- [ ] **Step 1: Write the failing tests**

增加以下测试：

```python
async def test_reservation_enforces_token_and_money_totals_and_disables_key(tenant_store):
    key = await tenant_store.create_client_key("alice", 1, 10, 100)
    model = await tenant_store.create_model("paid", "provider", "paid", 5_000_000, 5_000_000, True)
    reservation = await tenant_store.reserve_for_key(key.id, model.id, 10, 0, 10, 100)
    await tenant_store.settle_for_key(reservation.id, 10, 0, 50)
    current = await tenant_store.get_client_key(key.id)
    assert current.enabled is False
    assert current.disabled_reason == "quota_exhausted"
    with pytest.raises(QuotaExceeded):
        await tenant_store.reserve_for_key(key.id, model.id, 1, 0, 1, 1)

async def test_key_concurrency_guard_is_scoped_per_key():
    guard = KeyConcurrencyRegistry()
    assert await guard.try_acquire("a", 1)
    assert not await guard.try_acquire("a", 1)
    assert await guard.try_acquire("b", 1)
    await guard.release("a")
    assert await guard.try_acquire("a", 1)
```

- [ ] **Step 2: Run tests to verify they fail**

运行 `pytest tests/test_tenant_store.py::test_reservation_enforces_token_and_money_totals_and_disables_key tests/test_limiter.py::test_key_concurrency_guard_is_scoped_per_key -q`，预期失败。

- [ ] **Step 3: Implement reservation and guard**

在 `tenant_store.py` 实现单事务 `reserve_for_key()`：锁定 Key、检查 enabled、Token 已用+预留、金额已用+预留，写入请求 reservation，并在任一实际结算达到总额度时更新 `enabled=0`。预留金额使用整数公式 `ceil((input_tokens*input_price + output_tokens*output_price) / 1_000_000)`。

实现 `settle_for_key()`，把 reserved 转为 charged，失败/中断仍按实际或保守回退用量结算，确保重复结算会抛出错误而不是重复扣量。

在 `limiter.py` 增加 `KeyConcurrencyRegistry`，内部按 Key ID 创建 `ConcurrencyGuard`，`try_acquire(key_id, limit)` 和 `release(key_id)` 均为非阻塞原子操作；保留现有全局 `ConcurrencyGuard`。

- [ ] **Step 4: Run tests to verify they pass**

运行 `pytest tests/test_tenant_store.py tests/test_limiter.py tests/test_db.py -q`，预期全部通过。

- [ ] **Step 5: Commit**

跳过提交。

### Task 4: 多供应商注册表、加密密钥和模型同步

**Files:**
- Create: `provider_registry.py`
- Modify: `upstream.py`
- Modify: `tenant_store.py`
- Modify: `app.py`
- Test: `tests/test_provider_registry.py`
- Test: `tests/test_upstream.py`

- [ ] **Step 1: Write the failing tests**

创建 `tests/test_provider_registry.py`：

```python
async def test_provider_api_key_is_encrypted_and_model_routes_to_provider(tenant_store, fake_http_client):
    provider = await tenant_store.create_provider("供应商A", "https://a.example", "secret-a")
    model = await tenant_store.create_model("a/chat", provider.id, "chat-a", 0, 0, True)
    registry = ProviderRegistry(tenant_store, "test-master-key", fake_http_client)
    client = await registry.client_for_model(model.id)
    assert client.api_key == "secret-a"
    assert "secret-a" not in (await tenant_store.raw_provider_row(provider.id))["encrypted_api_key"]
```

增加同步失败隔离测试：A 供应商 `/v1/models` 返回两个模型时更新 A；B 返回 502 时保留 B 已有模型并返回失败状态。

- [ ] **Step 2: Run tests to verify they fail**

运行 `pytest tests/test_provider_registry.py -q`，预期失败。

- [ ] **Step 3: Implement registry and synchronization**

在 `provider_registry.py` 用 `Fernet(base64.urlsafe_b64encode(sha256(master_key).digest()))` 封装加解密；`ProviderRegistry` 缓存每个启用供应商的 `UpstreamClient`，供应商配置变更时替换客户端，旧客户端等租约结束后关闭。`client_for_model()` 只接受数据库模型 ID，不接受客户端传入 URL。

在 `tenant_store.py` 添加供应商 CRUD、模型 CRUD、模型公开别名改名、启停和按供应商同步导入；同步首次使用 `<provider-name>/<upstream-name>` 作为公开名，已存在模型保留管理员价格/启用状态。

在 `app.py` 的后台同步任务中遍历启用供应商，单个失败只记录脱敏警告；删除旧的单供应商同步路径。`upstream.py` 保持 OpenAI Chat Completions 和 `/v1/models` 传输实现，但把 `api_key` 暴露为仅测试可用的只读属性，不进入响应或日志。

- [ ] **Step 4: Run tests to verify they pass**

运行 `pytest tests/test_provider_registry.py tests/test_upstream.py -q`，预期通过。

- [ ] **Step 5: Commit**

跳过提交。

### Task 5: 接入客户端 Key 认证、模型组授权和请求结算

**Files:**
- Modify: `app.py`
- Modify: `runtime.py`
- Modify: `admin_api.py`
- Test: `tests/test_acceptance.py`
- Test: `tests/test_admin.py`

- [ ] **Step 1: Write the failing tests**

增加端到端行为：

```python
async def test_each_client_sees_only_authorized_models(client, tenant_store):
    basic_key = await seed_key_with_group(tenant_store, "basic", ["basic"])
    response = await client.get("/v1/models", headers={"Authorization": f"Bearer {basic_key}"})
    assert [row["id"] for row in response.json()["data"]] == ["basic"]

async def test_model_request_routes_by_public_alias_and_disables_on_quota(client, tenant_store, fake_upstreams):
    key = await seed_key_with_limits(tenant_store, token_limit=100, money_limit=100)
    response = await client.post("/v1/chat/completions", headers={"Authorization": f"Bearer {key}"}, json=chat_payload("provider-a/chat"))
    assert response.status_code == 200
    assert fake_upstreams["provider-a"].last_payload["model"] == "chat-internal"
```

增加停用 Key、未定价模型、无组权限、Key 并发超限和供应商失败的错误码测试。

- [ ] **Step 2: Run tests to verify they fail**

运行 `pytest tests/test_acceptance.py tests/test_admin.py -q`，预期新增测试失败，而既有单 Key 测试可能需要先标记为迁移兼容失败点。

- [ ] **Step 3: Implement request path changes**

在 `app.py` 用 `TenantStore.authenticate_key()` 替换 `_authorized()`；请求开始后获得 `ClientKeyLease`，通过 `authorized_models()` 查找公开模型和供应商，再调用 Key 并发守卫与 `reserve_for_key()`。`/v1/models` 只返回当前 Key 可用的已启用、已定价模型。

将 `chat_completions()` 的上游调用改为模型绑定的 `UpstreamClient`，继续保留现有 JSON/SSE、输入估算、全局 RPM/并发和错误格式。非流式与流式 finally 均释放 Key 并发槽并调用 `settle_for_key()`；结算达到额度时后续认证立即返回 401。

在 `runtime.py` 增加多供应商注册表租约的关闭流程；管理配置热加载只处理服务级设置和加密主密钥，供应商配置通过 `TenantStore` CRUD 生效。

- [ ] **Step 4: Run tests to verify they pass**

先运行 `pytest tests/test_acceptance.py tests/test_admin.py -q`，再运行 `pytest -q`。预期新增和原有测试全部通过。

- [ ] **Step 5: Commit**

跳过提交。

### Task 6: 管理 API 与中文控制台

**Files:**
- Modify: `admin_api.py`
- Modify: `admin_ui.py`
- Test: `tests/test_admin.py`

- [ ] **Step 1: Write the failing tests**

增加管理 API 测试：

```python
async def test_admin_can_create_key_once_and_manage_groups_providers(admin_client):
    created = await admin_client.post("/admin/api/keys", json={"name": "Alice", "max_concurrency": 2, "token_limit_total": 1000, "money_limit_yuan_total": 3})
    assert created.status_code == 201
    assert created.json()["token"].startswith("crk_")
    listed = await admin_client.get("/admin/api/keys")
    assert listed.json()["data"][0]["token"] is None

def test_admin_html_contains_multi_tenant_sections():
    html = ADMIN_HTML
    assert "供应商" in html
    assert "模型组" in html
    assert "客户端 Key" in html
    assert "一次性 Token 总额度" in html
```

- [ ] **Step 2: Run tests to verify they fail**

运行 `pytest tests/test_admin.py -q`，预期新增断言失败。

- [ ] **Step 3: Implement management resources and UI**

在 `admin_api.py` 增加 `/admin/api/providers`、`/admin/api/models`、`/admin/api/groups`、`/admin/api/keys` 的列表/创建/更新/删除和成员编辑路由。所有路由沿用 `X-Admin-Key`，返回结构只含脱敏密钥、前缀、状态、额度和用量；危险操作返回 409 或要求 `confirm=true`。

在 `admin_ui.py` 增加四个中文页面区域和原生 `fetch` 函数：供应商表单支持地址、上游 Key、启停和同步；模型表单支持公开别名、输入/输出元/百万 Token 价格、启停；模型组支持成员勾选；客户端 Key 支持一次性显示、并发/Token/金额额度和分组更换。所有英文技术字段以 `<small>` 中文注释方式保留，创建 Key 明文只保存在 JS 内存变量。

- [ ] **Step 4: Run tests to verify they pass**

运行 `pytest tests/test_admin.py -q`，预期通过；再用 HTML 静态断言确认没有 `localStorage`、`UPSTREAM_API_KEY` 明文回显或 token 持久化代码。

- [ ] **Step 5: Commit**

跳过提交。

### Task 7: 幂等迁移、部署文件和文档

**Files:**
- Modify: `app.py`
- Modify: `tenant_store.py`
- Modify: `.env.example`
- Modify: `docker-compose.yml`
- Modify: `Dockerfile`
- Modify: `ai-relay.service`
- Modify: `civic-relay.service`
- Modify: `README.md`
- Modify: `tests/test_deployment.py`
- Create: `scripts/init_config.ps1`

- [ ] **Step 1: Write the failing tests**

增加部署断言：

```python
def test_deployment_uses_external_config_path():
    script = (ROOT / "scripts" / "init_config.ps1").read_text(encoding="utf-8")
    assert "C:\\ProgramData\\CivicRelay\\relay.env" in script
    compose = (ROOT / "docker-compose.yml").read_text(encoding="utf-8")
    assert "C:/ProgramData/CivicRelay/relay.env:/app/.env:ro" in compose
```

增加迁移测试：重复调用 `initialize_legacy_data()` 不产生重复供应商、模型或兼容 Key。

- [ ] **Step 2: Run tests to verify they fail**

运行 `pytest tests/test_deployment.py -q`，预期失败。

- [ ] **Step 3: Implement migration and deployment**

在 `tenant_store.py` 实现幂等 `initialize_legacy_data(settings)`：从现有 Settings 创建默认供应商、导入模型白名单/映射和旧版兼容 Key；若数据库已有同名资源则跳过。启动 lifespan 在处理请求前调用一次。

创建 `scripts/init_config.ps1`：创建 `C:\ProgramData\CivicRelay`，从 `.env.example` 生成随机 Fernet 主密钥和随机管理/公共密钥，使用 `icacls` 移除 `Users`/`Authenticated Users` 的继承权限，仅保留当前用户、Administrators 和 SYSTEM；脚本不得把密钥输出到控制台。

Docker Compose 改为从宿主机 C 盘只读挂载配置、数据目录单独挂载；Windows 本地启动和 Linux systemd 启动参数均显式使用外部配置路径。Dockerfile 不复制任何 `.env`。

- [ ] **Step 4: Run tests to verify they pass**

运行 `pytest tests/test_deployment.py -q`，然后执行 `python -m compileall app.py config.py config_store.py db.py limiter.py tenant_store.py provider_registry.py`。

- [ ] **Step 5: Commit**

跳过提交。

### Task 8: 全量验证和安全回归

**Files:**
- Modify: `tests/test_acceptance.py` only if regression coverage needs fixture updates
- Modify: `README.md` only if commands differ after verification

- [ ] **Step 1: Run focused suites**

运行 `pytest tests/test_tenant_store.py tests/test_provider_registry.py tests/test_acceptance.py tests/test_admin.py -q`，确认多租户路径、供应商路由和管理面通过。

- [ ] **Step 2: Run the full suite**

运行 `pytest -q`，要求零失败；再运行 `python -m compileall app.py config.py config_store.py db.py limiter.py tenant_store.py provider_registry.py`。

- [ ] **Step 3: Perform runtime smoke checks without printing secrets**

使用 C 盘配置启动服务，验证 `/healthz` 为 200、无效 Key 为 401、有效 Key 的 `/v1/models` 只返回授权模型、额度耗尽后 Key 变为停用；检查日志输出中不包含 `Bearer`、供应商 Key 或客户端 Key 原文。

- [ ] **Step 4: Update documentation**

README 增加局域网分发流程：创建 Key、复制一次性密钥、设置客户端 `base_url` 为 `/v1`、模型组和额度管理、供应商同步、C 盘配置位置及单进程限制。明确多进程部署前必须使用共享限流器。

### Task 9: 邮箱白名单注册、用户身份与名额控制

**Goal:** 使用受邀邮箱和一次性验证码建立独立用户身份；客户端 Key 归属到用户，而不是把邮箱或验证码用于每次模型调用认证。

**Security decisions:**

- 用户永久标识使用 UUID，邮箱只作为唯一登录/邀请地址，不能作为主键。
- 仅白名单中的 `invited` 邮箱允许申请验证码；不得开放匿名注册。
- 验证码仅保存哈希、用途、过期时间、尝试次数和已使用标记，明文绝不入库、入日志或出现在 API 响应中。
- `MAX_ACTIVE_USERS` 限制已激活用户数，在验证码验证成功、账号即将从 `invited` 激活时以 SQLite 事务原子检查；达到上限时保留邀请状态并拒绝激活。
- `MAX_PENDING_INVITES` 限制待注册白名单数量；验证码发送次数另按邮箱和 IP 限制，不能与用户名额混用。
- 模型调用继续只使用客户端 Key。用户会话仅用于管理自己的 Key、使用记录和个人资料；供应商、全局配置和其他用户资源只允许管理员操作。

**Files:**
- Create: `identity_store.py`
- Create: `email_sender.py`
- Modify: `tenant_store.py`
- Modify: `app.py`
- Modify: `admin_api.py`
- Modify: `admin_ui.py`
- Modify: `config.py`
- Modify: `.env.example`
- Modify: `README.md`
- Create: `tests/test_identity_store.py`
- Modify: `tests/test_admin.py`
- Modify: `tests/test_acceptance.py`

- [ ] 建立 `users` 和 `email_verification_codes` 表。
  - `users`: `id` (UUID), `email`, `status` (`invited` / `active` / `disabled`), `role` (`user` / `admin`), `verified_at`, `created_at`。
  - `email_verification_codes`: 邮箱、用途、哈希、过期时间、尝试次数、使用时间、请求 IP、创建时间。
  - 为 `client_keys` 增加可空 `user_id`，历史 Key 归属到系统管理员/兼容用户，保证迁移幂等。

- [ ] 配置邮件服务和账号上限。
  - 增加 `MAX_ACTIVE_USERS`、`MAX_PENDING_INVITES`、验证码有效期、同邮箱冷却时间、邮箱/IP 每日发送上限。
  - 增加 SMTP（STARTTLS）或事务邮件服务配置；邮件凭据作为 C 盘外置配置中的机密，不在管理台回显。
  - 开发环境使用显式的测试邮件发送器，不允许把验证码写入应用日志。

- [ ] 实现管理员邀请和用户验证码端点。
  - 管理员创建/停用邀请白名单，创建时原子检查待邀请名额。
  - `POST /auth/email-codes` 无论邮箱是否存在均返回统一响应；仅合格白名单实际发送邮件。
  - `POST /auth/email-codes/verify` 原子校验哈希、用途、过期、尝试次数和一次性使用标记；激活前检查 `MAX_ACTIVE_USERS`。
  - 达到激活上限返回明确业务错误但不消费邀请码；已停用用户不占活动名额。

- [ ] 建立用户会话与资源边界。
  - 验证成功后签发短期、HttpOnly、Secure、SameSite 会话 Cookie，或等价短期令牌；不把管理员 API Key 下发给普通用户。
  - 新增用户自助端点，仅允许列出、创建、停用其自己的客户端 Key，并受管理员分配的模型组/额度模板约束。
  - 管理员仍可跨用户管理 Key、模型组、供应商和全局限制。

- [ ] 增加中文管理界面。
  - 管理员界面显示活动用户数、待邀请数、名额上限、邀请邮箱和状态，不显示验证码或邮件服务凭据。
  - 用户界面仅显示本人 Key；创建 Key 后仍只一次性展示原文。

- [ ] 编写安全和并发测试。
  - 相同邮箱大小写归一后只能对应一个 User ID。
  - 过期、已使用、超尝试次数的验证码无法验证；验证码明文不出现在数据库/API/日志。
  - 并发验证最后一个名额时仅一个账号可激活。
  - 非白名单邮箱、超邮件频率、超待邀请名额和超活动用户名额分别得到正确响应。
  - 普通用户无法读取或修改他人 Key、供应商、模型、模型组和全局配置。
