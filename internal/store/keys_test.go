package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewKeyIsReturnedOnceAndStoredAsDigest(t *testing.T) {
	repo := newTestStore(t)
	key, err := repo.CreateClientKey(context.Background(), NewClientKey{Name: "alice", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key.Token, "crk_") {
		t.Fatalf("token = %q", key.Token)
	}
	rows, err := repo.ListClientKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join([]string{rows[0].Token, rows[0].Name}, " "), key.Token) || rows[0].Token != "" {
		t.Fatal("plaintext token persisted or listed")
	}
	var digest []byte
	if err := repo.db.QueryRow("SELECT token_digest FROM client_keys WHERE id = ?", key.ID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if string(digest) == key.Token || len(digest) == 0 {
		t.Fatal("invalid token digest")
	}
}

// 结算必须幂等：同一笔预留的相同结果可以重复提交，不能重复计费。
// 注意这里刻意不再断言"Key 用满即自动停用"——v0.3.2 起已移除该行为（旧行为会
// 让运维调大限额后 Key 依然用不了），额度判定改由每次请求时进行。
func TestSettlementIsIdempotent(t *testing.T) {
	repo, key, model := newReservationFixture(t, 1000, 1_000_000)
	reservation, err := repo.ReserveRequest(context.Background(), newReservationArgs(key.ID, model.ID, "request-idempotent", 10))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SettleRequest(context.Background(), reservation.ID, 3, 4, 0, 700, "completed"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SettleRequest(context.Background(), reservation.ID, 3, 4, 0, 700, "completed"); err != nil {
		t.Fatalf("same settlement should be idempotent: %v", err)
	}
	current, err := repo.GetClientKey(context.Background(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Enabled {
		t.Fatalf("settlement must not disable the key any more: %#v", current)
	}
}

// TestKeylessCredentialHasNoUsableSecret 钉住「应用型凭据」的安全性质 —— 它存在的
// 全部意义就是「客户端里没有可提取的凭据」：
//   - 创建它不产生任何可用密钥（不返回 token，库里存的是丢弃的随机值）；
//   - 只能凭公开的应用标识认证；
//   - 密钥型 Key 的应用标识为空，不能被应用标识这条路冒充（否则是权限提升）；
//   - 到期语义与密钥型一致。
func TestKeylessCredentialHasNoUsableSecret(t *testing.T) {
	ctx := context.Background()
	repo := newTestStore(t)

	app, err := repo.CreateClientKey(ctx, NewClientKey{Name: "我的软件", Enabled: true, ConcurrencyLimit: 1, Keyless: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(app.PublicID, "app_") {
		t.Fatalf("应用型凭据应生成 app_ 前缀的公开标识，实际 %q", app.PublicID)
	}
	if app.Token != "" {
		t.Fatalf("应用型凭据不得返回任何密钥，实际 %q", app.Token)
	}
	if !app.Keyless() {
		t.Fatal("Keyless() 应为 true")
	}
	authenticated, err := repo.AuthenticateClientApp(ctx, app.PublicID)
	if err != nil || authenticated.ID != app.ID {
		t.Fatalf("按应用标识应能认证：id=%d err=%v", authenticated.ID, err)
	}

	normal, err := repo.CreateClientKey(ctx, NewClientKey{Name: "普通", Enabled: true, ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if normal.PublicID != "" {
		t.Fatalf("密钥型 Key 不应有应用标识，实际 %q", normal.PublicID)
	}
	if _, err := repo.AuthenticateClientApp(ctx, normal.Token); err == nil {
		t.Fatal("拿密钥当应用标识用必须失败（否则是权限提升）")
	}
	if _, err := repo.AuthenticateClientApp(ctx, ""); err == nil {
		t.Fatal("空应用标识必须失败")
	}

	past := time.Now().UTC().Add(-time.Minute)
	expiredApp, err := repo.CreateClientKey(ctx, NewClientKey{Name: "过期应用", Enabled: true, ConcurrencyLimit: 1, Keyless: true, ExpiresAt: &past})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AuthenticateClientApp(ctx, expiredApp.PublicID); !errors.Is(err, ErrClientKeyExpired) {
		t.Fatalf("过期的应用型凭据应返回 ErrClientKeyExpired，实际 %v", err)
	}
}

func ptrInt64(v int64) *int64 { return &v }

// TestClientKeyExpiryBlocksAuthentication 钉住「短时令牌」的语义：
//   - 到期的令牌认证时返回 ErrClientKeyExpired（而不是含混的「查不到」），
//     上层才能回 401 token_expired 让客户端知道该换新令牌；
//   - 不传 ExpiresAt 的 Key 永久有效 —— 升级后历史 Key 的性质不能变；
//   - 只改名字/并发不得顺手清掉有效期（否则「重命名」会变成「永久续期」）；
//   - 续期（ClearExpiresAt / 新 ExpiresAt）后立即恢复可用。
func TestClientKeyExpiryBlocksAuthentication(t *testing.T) {
	ctx := context.Background()
	repo := newTestStore(t)

	past := time.Now().UTC().Add(-time.Minute)
	expired, err := repo.CreateClientKey(ctx, NewClientKey{Name: "短时", Enabled: true, ConcurrencyLimit: 1, ExpiresAt: &past})
	if err != nil {
		t.Fatal(err)
	}
	if !expired.Expired(time.Now().UTC()) {
		t.Fatal("已过期的 Key，Expired 应为 true")
	}
	if _, err := repo.AuthenticateClientKey(ctx, expired.Token); !errors.Is(err, ErrClientKeyExpired) {
		t.Fatalf("过期令牌应返回 ErrClientKeyExpired，实际 %v", err)
	}

	future := time.Now().UTC().Add(time.Hour)
	fresh, err := repo.CreateClientKey(ctx, NewClientKey{Name: "有效", Enabled: true, ConcurrencyLimit: 1, ExpiresAt: &future})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AuthenticateClientKey(ctx, fresh.Token); err != nil {
		t.Fatalf("未过期的短时令牌应当可用：%v", err)
	}

	forever, err := repo.CreateClientKey(ctx, NewClientKey{Name: "永久", Enabled: true, ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if forever.ExpiresAt != nil {
		t.Fatalf("不传 ExpiresAt 必须表示永久有效，实际 %v", forever.ExpiresAt)
	}
	if _, err := repo.AuthenticateClientKey(ctx, forever.Token); err != nil {
		t.Fatal(err)
	}

	// 改名（不带 ExpiresAt / ClearExpiresAt）不得改变有效期
	if _, err := repo.UpdateClientKey(ctx, fresh.ID, UpdateClientKey{Name: "改名了"}); err != nil {
		t.Fatal(err)
	}
	renamed, err := repo.AuthenticateClientKey(ctx, fresh.Token)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ExpiresAt == nil {
		t.Fatal("改名不应清掉有效期")
	}
	if renamed.Name != "改名了" {
		t.Fatalf("改名未生效：%q", renamed.Name)
	}

	// 续期：改为永久后立即可用
	if _, err := repo.UpdateClientKey(ctx, expired.ID, UpdateClientKey{ClearExpiresAt: true}); err != nil {
		t.Fatal(err)
	}
	restored, err := repo.AuthenticateClientKey(ctx, expired.Token)
	if err != nil {
		t.Fatalf("改为永久后应当恢复可用：%v", err)
	}
	if restored.ExpiresAt != nil {
		t.Fatal("ExpiresAt 应已清空")
	}

	// 续期：重设一个新时间点
	later := time.Now().UTC().Add(2 * time.Hour)
	if _, err := repo.UpdateClientKey(ctx, expired.ID, UpdateClientKey{ExpiresAt: &later}); err != nil {
		t.Fatal(err)
	}
	renewed, err := repo.AuthenticateClientKey(ctx, expired.Token)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.ExpiresAt == nil || renewed.ExpiresAt.Before(later.Add(-time.Second)) {
		t.Fatalf("续期未生效：%v", renewed.ExpiresAt)
	}
}

// TestAuthorizedModelsForKeyMatchesTokenLookup 钉住两件事：
//  1. 按 Key ID 查出的授权模型与按明文 token 查出的完全一致 —— 管理台「接入信息」
//     与公共 /v1/models 共用 authorizedModelsSelect 一份 SQL，不允许出现
//     「管理台说已授权、客户端却拉不到」这种最难查的漂移；
//  2. Key 停用后按 ID 仍能查出它本来会拿到什么 —— 否则运维无法从接入信息判断
//     客户端 401 到底是「Key 被停用」还是「模型没授权」。
func TestAuthorizedModelsForKeyMatchesTokenLookup(t *testing.T) {
	ctx := context.Background()
	repo := newTestStore(t)
	price := int64(1_000_000)

	provider, err := repo.CreateProvider(ctx, NewProvider{Name: "p", BaseURL: "https://provider.example", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct {
		name   string
		priced bool
		on     bool
	}{
		{"priced", true, true},
		{"unpriced", false, true},
		{"disabled", true, false},
	} {
		model := NewModel{ProviderID: provider.ID, PublicName: spec.name, UpstreamName: spec.name, Enabled: spec.on}
		if spec.priced {
			model.InputPriceMicroyuan, model.OutputPriceMicroyuan = &price, &price
		}
		if _, err := repo.CreateModel(ctx, model); err != nil {
			t.Fatal(err)
		}
	}
	group, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "客户端组", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceGroupProviders(ctx, group.ID, []int64{provider.ID}); err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "其他软件", Enabled: true, ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceKeyGroups(ctx, key.ID, []int64{group.ID}); err != nil {
		t.Fatal(err)
	}

	byToken, err := repo.AuthorizedModels(ctx, key.Token)
	if err != nil {
		t.Fatal(err)
	}
	if len(byToken) != 1 || byToken[0].PublicName != "priced" {
		t.Fatalf("按 token 查询应只授权「已启用且已定价」的模型：%v", byToken)
	}
	byID, err := repo.AuthorizedModelsForKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(byID) != len(byToken) || byID[0].PublicName != byToken[0].PublicName {
		t.Fatalf("按 ID 与按 token 查询结果必须一致：%v vs %v", byID, byToken)
	}

	// 停用 Key：按 ID 仍应看得到模型清单（接入信息据此提示 401 的成因），
	// 而公共接口必须彻底查不到。
	off := false
	if _, err := repo.UpdateClientKey(ctx, key.ID, UpdateClientKey{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if models, err := repo.AuthorizedModelsForKey(ctx, key.ID); err != nil || len(models) != 1 {
		t.Fatalf("停用后按 ID 仍应能预览模型：%v %v", models, err)
	}
	if models, err := repo.AuthorizedModels(ctx, key.Token); err != nil || len(models) != 0 {
		t.Fatalf("停用后按 token 必须查不到任何模型：%v %v", models, err)
	}
}

func TestSetKeyGroupBindsUnbindsAndValidates(t *testing.T) {
	ctx := context.Background()
	repo := newTestStore(t)
	g, err := repo.CreateModelGroup(ctx, NewModelGroup{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := repo.CreateClientKey(ctx, NewClientKey{Name: "k", ConcurrencyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	// 新建的 Key 未绑分组
	if fresh, _ := repo.GetClientKey(ctx, key.ID); fresh.GroupID != nil {
		t.Fatalf("新建 Key 不应有分组：%v", fresh.GroupID)
	}

	if err := repo.SetKeyGroup(ctx, key.ID, g.ID); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetClientKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.GroupID == nil || *got.GroupID != g.ID {
		t.Fatalf("group = %v, want %d", got.GroupID, g.ID)
	}

	// 解绑：0 = 解绑，授权模型随即为空
	if err := repo.SetKeyGroup(ctx, key.ID, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ = repo.GetClientKey(ctx, key.ID); got.GroupID != nil {
		t.Fatalf("解绑后 group = %v, want nil", got.GroupID)
	}

	// 不存在的组
	if err := repo.SetKeyGroup(ctx, key.ID, 99999); err == nil {
		t.Fatal("不存在的组被接受")
	}
	// 停用的组（与旧 ReplaceKeyGroups 的校验一致）
	if _, err := repo.UpdateModelGroup(ctx, g.ID, UpdateModelGroup{Enabled: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetKeyGroup(ctx, key.ID, g.ID); err == nil {
		t.Fatal("停用的组被接受")
	}
	// 不存在的 Key
	if err := repo.SetKeyGroup(ctx, 99999, g.ID); err == nil {
		t.Fatal("不存在的 Key 被接受")
	}
}
