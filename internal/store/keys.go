package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrClientKeyExpired 表示「令牌存在且格式正确，但已超过有效期」。
// 与「查不到/被停用」分开，是为了让客户端能区分这两种情况：前者该换新令牌，
// 后者是凭据本身不对，混在一起会让调用方一直重试一个永远不会好的请求。
var ErrClientKeyExpired = errors.New("client key expired")

type ClientKey struct {
	ID                   int64
	Name                 string
	Token                string // populated only by CreateClientKey / RotateClientKey
	TokenLabel           string // 列表展示用的非敏感标识，例如 key-9fA2c1
	TokenHint            string // 脱敏缩略，例如 crk_Vm8U…（仅由专用接口返回）
	Enabled              bool
	ConcurrencyLimit     int
	TokenLimit           *int64
	AmountLimitMicroyuan *int64
	DisabledReason       string
	// ExpiresAt 为 nil 表示永久有效。非 nil 时到点即失效（认证返回 ErrClientKeyExpired）。
	// 这是「短时令牌」的实现：给程序/客户端发一个会自己过期的令牌，泄露后的可用窗口
	// 有限，且到期自动失效，不依赖人工吊销。
	ExpiresAt *time.Time
	// PublicID 非空表示这是「应用型凭据」：用请求头 X-App-Id 接入，且**没有可用密钥**
	// （token_digest 是创建时即丢弃的随机值）。客户端里没有可提取的凭据，用户拿不到
	// 东西带到应用外去用。应用标识本身是公开的，安全边界在「谁能访问到网关」。
	PublicID string
	// GroupID 为 nil 表示未绑分组（授权模型为空，客户端拉不到模型）。
	// 一对一：一个 Key 只属于一个分组，切换即改绑；历史账目按请求发生时的
	// 归属记录在 key_reservations.group_id / requests.group_id 上，不随切换搬移。
	GroupID *int64
}

// Keyless 报告该记录是否为「应用型凭据」（无密钥，用公开的应用标识接入）。
func (k ClientKey) Keyless() bool { return strings.TrimSpace(k.PublicID) != "" }

// Expired 报告该 Key 在给定时刻是否已因超过有效期而失效。永久有效的 Key 永远返回 false。
func (k ClientKey) Expired(now time.Time) bool {
	return k.ExpiresAt != nil && !now.Before(*k.ExpiresAt)
}

type NewClientKey struct {
	Name                 string
	Enabled              bool
	ConcurrencyLimit     int
	TokenLimit           *int64
	AmountLimitMicroyuan *int64
	// ExpiresAt 为 nil = 永久有效。
	ExpiresAt *time.Time
	// Keyless 为 true 时创建「应用型凭据」：服务端生成公开的应用标识（app_…），
	// 不产生任何可用密钥。给那些「客户端不能保存密钥」的软件用。
	Keyless bool
}

type UpdateClientKey struct {
	Name                 string
	Enabled              *bool
	ConcurrencyLimit     *int
	TokenLimit           *int64
	AmountLimitMicroyuan *int64
	DisabledReason       *string
	// ExpiresAt 非 nil 时把有效期设为该时刻（「从现在起 N 秒后」由调用方算好）。
	// ClearExpiresAt 为 true 时改为永久有效；两者同时给出时以 ClearExpiresAt 为准。
	ExpiresAt      *time.Time
	ClearExpiresAt bool
}

func (s *Store) CreateClientKey(ctx context.Context, in NewClientKey) (ClientKey, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return ClientKey{}, errors.New("client key name is required")
	}
	if in.ConcurrencyLimit <= 0 {
		return ClientKey{}, errors.New("concurrency limit must be positive")
	}
	if err := validateLimit(in.TokenLimit, "token limit"); err != nil {
		return ClientKey{}, err
	}
	if err := validateLimit(in.AmountLimitMicroyuan, "amount limit"); err != nil {
		return ClientKey{}, err
	}
	token, err := randomClientToken()
	if err != nil {
		return ClientKey{}, err
	}
	label, hint := tokenLabel(), tokenHint(token)
	// ⚠️ 摘要必须用**真实的随机 token** 算，且要在丢弃明文之前算：
	// 应用型凭据对外不返回密钥，但如果拿空串去算摘要，所有应用型凭据会得到同一个
	// 摘要值，第二条就撞 token_digest 的 UNIQUE 约束（曾经真的这么写过一次）。
	digest := s.box.DigestBytes(token)
	// 应用型凭据：生成公开的应用标识，并把刚生成的密钥明文彻底丢弃（不写进任何
	// 返回值、不落盘、不打日志）—— 于是「用密钥认证」这条路对它是关闭的，
	// 客户端里也就没有可提取的东西。
	var publicID any
	if in.Keyless {
		generated, err := newPublicID()
		if err != nil {
			return ClientKey{}, err
		}
		publicID = generated
		token, hint = "", ""
	}
	// 客户端 Key 一律以启用状态创建；停用通过 UpdateClientKey 显式操作。
	enabled := 1
	now := nowUTC()
	// nil 接口值写进 SQLite 就是 NULL，正好表示「永久有效」
	var expiry any
	if in.ExpiresAt != nil {
		expiry = in.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO client_keys(name,token_digest,token_label,token_hint,enabled,concurrency_limit,token_limit,amount_limit_microyuan,expires_at_utc,public_id,created_at_utc,updated_at_utc) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, name, digest, label, hint, enabled, in.ConcurrencyLimit, in.TokenLimit, in.AmountLimitMicroyuan, expiry, publicID, now, now)
	if err != nil {
		return ClientKey{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ClientKey{}, err
	}
	publicIDText, _ := publicID.(string)
	return ClientKey{ID: id, Name: name, Token: token, TokenLabel: label, TokenHint: hint, Enabled: true, ConcurrencyLimit: in.ConcurrencyLimit, TokenLimit: in.TokenLimit, AmountLimitMicroyuan: in.AmountLimitMicroyuan, ExpiresAt: in.ExpiresAt, PublicID: publicIDText}, nil
}

// newPublicID 生成公开的应用标识，形如 app_<22 位随机>。它是**公开**的（会写在
// 客户端里），只用来告诉服务端「我是哪个应用」，本身不是秘密 —— 所以它也不是安全
// 边界，边界在「谁能访问到这个网关」。
func newPublicID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "app_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// RotateClientKey issues a brand new token for an existing key and invalidates
// the previous one. The plaintext is returned exactly once, like on creation.
func (s *Store) RotateClientKey(ctx context.Context, id int64) (ClientKey, error) {
	if id <= 0 {
		return ClientKey{}, errors.New("client key ID is required")
	}
	if _, err := s.GetClientKey(ctx, id); err != nil {
		return ClientKey{}, err
	}
	token, err := randomClientToken()
	if err != nil {
		return ClientKey{}, err
	}
	label, hint := tokenLabel(), tokenHint(token)
	if _, err := s.db.ExecContext(ctx, `UPDATE client_keys SET token_digest=?, token_label=?, token_hint=?, updated_at_utc=? WHERE id=?`, s.box.DigestBytes(token), label, hint, nowUTC(), id); err != nil {
		return ClientKey{}, err
	}
	updated, err := s.GetClientKey(ctx, id)
	if err != nil {
		return ClientKey{}, err
	}
	updated.Token = token
	updated.TokenHint = hint
	return updated, nil
}

// DeleteClientKey removes a key permanently, revoking access immediately.
// Reservations cascade; historical requests keep their billing amount but lose
// the key reference, so past ledger totals stay intact.
func (s *Store) DeleteClientKey(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("client key ID is required")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM client_keys WHERE id=?`, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// KeyTokenHints returns the masked prefix (e.g. "crk_Vm8U…") of every client
// key, keyed by ID. It is served through a dedicated endpoint so the key
// listing itself stays free of any secret material.
func (s *Store) KeyTokenHints(ctx context.Context) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(token_hint,'') FROM client_keys`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]string)
	for rows.Next() {
		var id int64
		var hint string
		if err := rows.Scan(&id, &hint); err != nil {
			return nil, err
		}
		out[id] = hint
	}
	return out, rows.Err()
}

// tokenLabel returns a short, non-secret identifier shown in the admin console.
// It is deliberately unrelated to the token itself: listings must never reveal
// any character of a client secret.
func tokenLabel() string {
	raw := make([]byte, 4)
	if _, err := rand.Read(raw); err != nil {
		return "key-" + strconv.FormatInt(time.Now().UnixNano()%0xffffff, 16)
	}
	return "key-" + base64.RawURLEncoding.EncodeToString(raw)
}

// tokenHint keeps a short masked prefix so operators can tell keys apart in the
// console. Only the first few characters are kept: the rest of the 47-character
// secret stays irrecoverable, and no listing endpoint exposes it.
func tokenHint(token string) string {
	const visible = 8 // "crk_" plus four random characters
	if len(token) <= visible {
		return token
	}
	return token[:visible] + "…"
}

func validateLimit(v *int64, label string) error {
	if v != nil && *v < 0 {
		return fmt.Errorf("%s must be non-negative", label)
	}
	return nil
}

func randomClientToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate client key: %w", err)
	}
	return "crk_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func (s *Store) ListClientKeys(ctx context.Context) ([]ClientKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,enabled,concurrency_limit,token_limit,amount_limit_microyuan,COALESCE(disabled_reason,''),COALESCE(token_label,''),expires_at_utc,COALESCE(public_id,''),group_id FROM client_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// 非 nil 空切片：空集合序列化为 [] 而非 null，避免客户端额外判空
	out := make([]ClientKey, 0)
	for rows.Next() {
		k, err := scanClientKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) GetClientKey(ctx context.Context, id int64) (ClientKey, error) {
	return scanClientKey(s.db.QueryRowContext(ctx, `SELECT id,name,enabled,concurrency_limit,token_limit,amount_limit_microyuan,COALESCE(disabled_reason,''),COALESCE(token_label,''),expires_at_utc,COALESCE(public_id,''),group_id FROM client_keys WHERE id=?`, id))
}

// KeyUsageStat mirrors the quota-check aggregation in ReserveRequest: every
// reservation counts, no matter how it finished, so the numbers operators see
// match the limits actually enforced.
type KeyUsageStat struct {
	Tokens          int64
	AmountMicroyuan int64
	Requests        int64
}

// KeyUsage returns consumed tokens and amount per key. 与 ReserveRequest 的
// 额度判定同口径：预留中的请求也算已用，否则运维看到的数字会比实际生效的宽松。
func (s *Store) KeyUsage(ctx context.Context) (map[int64]KeyUsageStat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key_id,
		COALESCE(SUM(charged_tokens+reserved_tokens),0),
		COALESCE(SUM(charged_amount_microyuan+reserved_amount_microyuan),0),
		COUNT(*)
		FROM key_reservations
		WHERE status IN ('reserved','completed','failed','aborted')
		GROUP BY key_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]KeyUsageStat)
	for rows.Next() {
		var keyID int64
		var stat KeyUsageStat
		if err := rows.Scan(&keyID, &stat.Tokens, &stat.AmountMicroyuan, &stat.Requests); err != nil {
			return nil, err
		}
		out[keyID] = stat
	}
	return out, rows.Err()
}

// AuthenticateClientKey resolves a plaintext client token to its redacted
// database record. The token is compared only through its keyed digest.
//
// 返回 ErrClientKeyExpired（连同 Key 记录）时，token 是**正确的**但已过期：
// 短时令牌到点即失效，调用方据此回 401 token_expired 而不是 invalid_api_key。
func (s *Store) AuthenticateClientKey(ctx context.Context, token string) (ClientKey, error) {
	if strings.TrimSpace(token) == "" {
		return ClientKey{}, errors.New("client token is required")
	}
	key, err := scanClientKey(s.db.QueryRowContext(ctx, `SELECT id,name,enabled,concurrency_limit,token_limit,amount_limit_microyuan,COALESCE(disabled_reason,''),COALESCE(token_label,''),expires_at_utc,COALESCE(public_id,''),group_id FROM client_keys WHERE token_digest=?`, s.box.DigestBytes(token)))
	if err != nil {
		return ClientKey{}, err
	}
	if key.Expired(time.Now().UTC()) {
		return key, ErrClientKeyExpired
	}
	return key, nil
}

// AuthenticateClientApp 按公开的应用标识（请求头 X-App-Id）解析出对应记录。
//
// 只有「应用型凭据」能走这条路：普通 Key 的 public_id 为空，查不到 —— 否则一个
// 公开的应用标识就能冒充持有密钥的 Key，那是权限提升。反向也一样：应用型凭据
// 的密钥明文已被丢弃，无法用 Bearer 认证。
func (s *Store) AuthenticateClientApp(ctx context.Context, publicID string) (ClientKey, error) {
	id := strings.TrimSpace(publicID)
	if id == "" {
		return ClientKey{}, errors.New("application ID is required")
	}
	key, err := scanClientKey(s.db.QueryRowContext(ctx, `SELECT id,name,enabled,concurrency_limit,token_limit,amount_limit_microyuan,COALESCE(disabled_reason,''),COALESCE(token_label,''),expires_at_utc,COALESCE(public_id,''),group_id FROM client_keys WHERE public_id=?`, id))
	if err != nil {
		return ClientKey{}, err
	}
	if key.Expired(time.Now().UTC()) {
		return key, ErrClientKeyExpired
	}
	return key, nil
}

func scanClientKey(src interface{ Scan(...any) error }) (ClientKey, error) {
	var k ClientKey
	var enabled int
	var tokens, amount, group sql.NullInt64
	var expires sql.NullString
	if err := src.Scan(&k.ID, &k.Name, &enabled, &k.ConcurrencyLimit, &tokens, &amount, &k.DisabledReason, &k.TokenLabel, &expires, &k.PublicID, &group); err != nil {
		return ClientKey{}, err
	}
	k.Enabled = enabled == 1
	k.TokenLimit, k.AmountLimitMicroyuan = nullableInt(tokens), nullableInt(amount)
	k.ExpiresAt = nullableTime(expires)
	if group.Valid {
		k.GroupID = &group.Int64
	}
	k.Token = ""
	return k, nil
}

// nullableTime 把库里的 RFC3339Nano 文本转成 *time.Time；NULL/空串视为「永久有效」。
// 解析失败也按永久处理：一个坏掉的过期时间不该把 Key 变成永远不可用 —— 那比
// 「没设过期」更糟，而且当事人几乎没有自证的手段。
func nullableTime(v sql.NullString) *time.Time {
	if !v.Valid || strings.TrimSpace(v.String) == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, v.String)
	if err != nil {
		return nil
	}
	utc := parsed.UTC()
	return &utc
}

func (s *Store) UpdateClientKey(ctx context.Context, id int64, in UpdateClientKey) (ClientKey, error) {
	k, err := s.GetClientKey(ctx, id)
	if err != nil {
		return ClientKey{}, err
	}
	if strings.TrimSpace(in.Name) != "" {
		k.Name = strings.TrimSpace(in.Name)
	}
	if in.Enabled != nil {
		k.Enabled = *in.Enabled
	}
	if in.ConcurrencyLimit != nil {
		if *in.ConcurrencyLimit <= 0 {
			return ClientKey{}, errors.New("concurrency limit must be positive")
		}
		k.ConcurrencyLimit = *in.ConcurrencyLimit
	}
	if in.TokenLimit != nil {
		if err := validateLimit(in.TokenLimit, "token limit"); err != nil {
			return ClientKey{}, err
		}
		k.TokenLimit = in.TokenLimit
	}
	if in.AmountLimitMicroyuan != nil {
		if err := validateLimit(in.AmountLimitMicroyuan, "amount limit"); err != nil {
			return ClientKey{}, err
		}
		k.AmountLimitMicroyuan = in.AmountLimitMicroyuan
	}
	if in.DisabledReason != nil {
		k.DisabledReason = *in.DisabledReason
	}
	// 有效期：ClearExpiresAt 优先（改回永久），否则 ExpiresAt 非 nil 就设成该时刻。
	// 传 nil 的 ExpiresAt 表示「不动有效期」，这样才能把「续期」和「只改名字」
	// 区分开 —— 否则任何一次 PUT 都会顺手清掉有效期。
	if in.ClearExpiresAt {
		k.ExpiresAt = nil
	} else if in.ExpiresAt != nil {
		expiry := in.ExpiresAt.UTC()
		k.ExpiresAt = &expiry
	}
	var expiry any
	if k.ExpiresAt != nil {
		expiry = k.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE client_keys SET name=?,enabled=?,concurrency_limit=?,token_limit=?,amount_limit_microyuan=?,disabled_reason=?,expires_at_utc=?,updated_at_utc=? WHERE id=?`, k.Name, boolInt(k.Enabled), k.ConcurrencyLimit, k.TokenLimit, k.AmountLimitMicroyuan, k.DisabledReason, expiry, nowUTC(), id)
	if err != nil {
		return ClientKey{}, err
	}
	return k, nil
}

// KeyGroupIDs returns the group IDs currently authorized for a client key,
// ordered by group ID. Used by the administration console to prefill editing.
func (s *Store) KeyGroupIDs(ctx context.Context, keyID int64) ([]int64, error) {
	if keyID <= 0 {
		return nil, errors.New("key ID is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT group_id FROM key_groups WHERE key_id=? ORDER BY group_id`, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// 非 nil 空切片：空集合序列化为 [] 而非 null，避免客户端额外判空
	out := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetKeyGroup 把密钥绑到唯一分组，groupID <= 0 表示解绑（授权模型随即为空）。
// 取代旧的 ReplaceKeyGroups 多对多写入；组必须存在且启用，与旧校验一致。
// 注意：Task 3 切换之前，授权/计价读路径（authorizedModelsSelect、ledger 计价）
// 仍走 key_groups，本函数暂不影响实际授权口径；ReplaceKeyGroups 暂勿删除。
func (s *Store) SetKeyGroup(ctx context.Context, keyID, groupID int64) error {
	if keyID <= 0 {
		return errors.New("key ID is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM client_keys WHERE id=?`, keyID).Scan(&exists); err != nil || exists == 0 {
		if err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	if groupID > 0 {
		var enabled int
		if err := tx.QueryRowContext(ctx, `SELECT enabled FROM model_groups WHERE id=?`, groupID).Scan(&enabled); err != nil {
			return err
		}
		if enabled != 1 {
			return fmt.Errorf("group %d is disabled", groupID)
		}
		_, err = tx.ExecContext(ctx, `UPDATE client_keys SET group_id=?, updated_at_utc=? WHERE id=?`, groupID, nowUTC(), keyID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE client_keys SET group_id=NULL, updated_at_utc=? WHERE id=?`, nowUTC(), keyID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ReplaceKeyGroups(ctx context.Context, keyID int64, groupIDs []int64) error {
	if keyID <= 0 {
		return errors.New("key ID is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM client_keys WHERE id=?`, keyID).Scan(&exists); err != nil || exists == 0 {
		if err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	seen := make(map[int64]struct{}, len(groupIDs))
	for _, gid := range groupIDs {
		if gid <= 0 {
			return errors.New("invalid group ID")
		}
		if _, ok := seen[gid]; ok {
			return errors.New("duplicate group ID")
		}
		seen[gid] = struct{}{}
		var enabled int
		if err := tx.QueryRowContext(ctx, `SELECT enabled FROM model_groups WHERE id=?`, gid).Scan(&enabled); err != nil {
			return err
		}
		if enabled != 1 {
			return fmt.Errorf("group %d is disabled", gid)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM key_groups WHERE key_id=?`, keyID); err != nil {
		return err
	}
	for _, gid := range groupIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO key_groups(key_id,group_id) VALUES (?,?)`, keyID, gid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// KeyModelRateMilli returns the billing rate multiplier (in thousandths, 1000 = 1.0x)
// that applies to a model for a given client key: the highest rate among the key's
// enabled groups whose providers include the model's provider. Falls back to the
// default rate when no matching group carries an explicit multiplier.
func (s *Store) KeyModelRateMilli(ctx context.Context, keyID, modelID int64) (int64, error) {
	if keyID <= 0 || modelID <= 0 {
		return DefaultGroupRateMilli, errors.New("key and model IDs are required")
	}
	var rate sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(g.rate_milli) FROM key_groups kg JOIN model_groups g ON g.id=kg.group_id AND g.enabled=1 JOIN group_providers gp ON gp.group_id=g.id JOIN models m ON m.id=? AND m.provider_id=gp.provider_id WHERE kg.key_id=?`, modelID, keyID).Scan(&rate)
	if err != nil {
		return DefaultGroupRateMilli, err
	}
	if !rate.Valid || rate.Int64 <= 0 {
		return DefaultGroupRateMilli, nil
	}
	return rate.Int64, nil
}

func (s *Store) AuthorizedModels(ctx context.Context, token string) ([]Model, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("client token is required")
	}
	// 组管理渠道：授权模型 = Key 启用组内渠道下、已定价且启用的模型。
	return s.queryAuthorizedModels(ctx, "k.token_digest=? AND k.enabled=1", s.box.DigestBytes(token))
}

// AuthorizedModelsForKey 与 AuthorizedModels 同口径，但按 Key ID 查询：管理端
// 拿不到明文 token，却需要把「其他软件该填哪些模型名」原样告诉运维。刻意不校验
// k.enabled —— 停用的 Key 也要能看到它本来会拿到什么，否则运维无法判断客户端的
// 401 是「Key 被停用」还是「模型没授权」。
func (s *Store) AuthorizedModelsForKey(ctx context.Context, keyID int64) ([]Model, error) {
	if keyID <= 0 {
		return nil, errors.New("key ID is required")
	}
	return s.queryAuthorizedModels(ctx, "k.id=?", keyID)
}

// authorizedModelsSelect 是「客户端实际能用哪些模型」的唯一口径：
// Key 所属启用分组 → 组内渠道 → 渠道下已启用且已定价的模型 → 渠道本身启用。
// 公共 /v1/models 与管理端「接入信息」共用这一份 SQL，避免两处判断漂移出
// 「客户端报 404、管理台却显示已授权」这类最难查的问题。
const authorizedModelsSelect = `SELECT DISTINCT m.id,m.provider_id,m.public_name,m.upstream_name,m.input_price_microyuan,m.output_price_microyuan,m.cached_input_price_microyuan,m.enabled FROM client_keys k JOIN key_groups kg ON kg.key_id=k.id JOIN model_groups g ON g.id=kg.group_id AND g.enabled=1 JOIN group_providers gp ON gp.group_id=g.id JOIN models m ON m.provider_id=gp.provider_id AND m.enabled=1 AND m.input_price_microyuan IS NOT NULL AND m.output_price_microyuan IS NOT NULL JOIN providers p ON p.id=m.provider_id AND p.enabled=1 WHERE `

func (s *Store) queryAuthorizedModels(ctx context.Context, where string, args ...any) ([]Model, error) {
	rows, err := s.db.QueryContext(ctx, authorizedModelsSelect+where+" ORDER BY m.id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// 非 nil 空切片：空集合序列化为 [] 而非 null，避免客户端额外判空
	out := make([]Model, 0)
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
