package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

type ClientKey struct {
	ID                   int64
	Name                 string
	Token                string // populated only by CreateClientKey
	Enabled              bool
	ConcurrencyLimit     int
	TokenLimit           *int64
	AmountLimitMicroyuan *int64
	DisabledReason       string
}

type NewClientKey struct {
	Name                 string
	Enabled              bool
	ConcurrencyLimit     int
	TokenLimit           *int64
	AmountLimitMicroyuan *int64
}

type UpdateClientKey struct {
	Name                 string
	Enabled              *bool
	ConcurrencyLimit     *int
	TokenLimit           *int64
	AmountLimitMicroyuan *int64
	DisabledReason       *string
}

type KeyReservation struct {
	ID                      int64
	KeyID                   int64
	ModelID                 int64
	ReservedTokens          int64
	ReservedAmountMicroyuan int64
	ChargedTokens           int64
	ChargedAmountMicroyuan  int64
	Status                  string
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
	enabled := 1
	if in.Enabled {
		enabled = 1
	}
	now := nowUTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO client_keys(name,token_digest,enabled,concurrency_limit,token_limit,amount_limit_microyuan,created_at_utc,updated_at_utc) VALUES (?,?,?,?,?,?,?,?)`, name, s.box.DigestBytes(token), enabled, in.ConcurrencyLimit, in.TokenLimit, in.AmountLimitMicroyuan, now, now)
	if err != nil {
		return ClientKey{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ClientKey{}, err
	}
	return ClientKey{ID: id, Name: name, Token: token, Enabled: true, ConcurrencyLimit: in.ConcurrencyLimit, TokenLimit: in.TokenLimit, AmountLimitMicroyuan: in.AmountLimitMicroyuan}, nil
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
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,enabled,concurrency_limit,token_limit,amount_limit_microyuan,COALESCE(disabled_reason,'') FROM client_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClientKey
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
	return scanClientKey(s.db.QueryRowContext(ctx, `SELECT id,name,enabled,concurrency_limit,token_limit,amount_limit_microyuan,COALESCE(disabled_reason,'') FROM client_keys WHERE id=?`, id))
}

func scanClientKey(src interface{ Scan(...any) error }) (ClientKey, error) {
	var k ClientKey
	var enabled int
	var tokens, amount sql.NullInt64
	if err := src.Scan(&k.ID, &k.Name, &enabled, &k.ConcurrencyLimit, &tokens, &amount, &k.DisabledReason); err != nil {
		return ClientKey{}, err
	}
	k.Enabled = enabled == 1
	k.TokenLimit, k.AmountLimitMicroyuan = nullableInt(tokens), nullableInt(amount)
	k.Token = ""
	return k, nil
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
	_, err = s.db.ExecContext(ctx, `UPDATE client_keys SET name=?,enabled=?,concurrency_limit=?,token_limit=?,amount_limit_microyuan=?,disabled_reason=?,updated_at_utc=? WHERE id=?`, k.Name, boolInt(k.Enabled), k.ConcurrencyLimit, k.TokenLimit, k.AmountLimitMicroyuan, k.DisabledReason, nowUTC(), id)
	if err != nil {
		return ClientKey{}, err
	}
	return k, nil
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

func (s *Store) AuthorizedModels(ctx context.Context, token string) ([]Model, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("client token is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT m.id,m.provider_id,m.public_name,m.upstream_name,m.input_price_microyuan,m.output_price_microyuan,m.enabled FROM client_keys k JOIN key_groups kg ON kg.key_id=k.id JOIN model_groups g ON g.id=kg.group_id AND g.enabled=1 JOIN group_models gm ON gm.group_id=g.id JOIN models m ON m.id=gm.model_id AND m.enabled=1 AND m.input_price_microyuan IS NOT NULL AND m.output_price_microyuan IS NOT NULL JOIN providers p ON p.id=m.provider_id AND p.enabled=1 WHERE k.token_digest=? AND k.enabled=1 ORDER BY m.id`, s.box.DigestBytes(token))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ReserveForKey(ctx context.Context, keyID, modelID, tokens, amount int64) (KeyReservation, error) {
	s.reservationMu.Lock()
	defer s.reservationMu.Unlock()
	if keyID <= 0 || modelID <= 0 || tokens < 0 || amount < 0 {
		return KeyReservation{}, errors.New("invalid reservation")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return KeyReservation{}, err
	}
	defer tx.Rollback()
	var enabled, concurrency int
	var tokenLimit, amountLimit sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT k.enabled,k.concurrency_limit,k.token_limit,k.amount_limit_microyuan FROM client_keys k WHERE k.id=?`, keyID).Scan(&enabled, &concurrency, &tokenLimit, &amountLimit); err != nil {
		return KeyReservation{}, err
	}
	if enabled != 1 {
		return KeyReservation{}, errors.New("client key is disabled")
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM key_reservations WHERE key_id=? AND status='reserved'`, keyID).Scan(&active); err != nil {
		return KeyReservation{}, err
	}
	if active >= concurrency {
		return KeyReservation{}, errors.New("concurrency limit exceeded")
	}
	var allowed int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM key_groups kg JOIN model_groups g ON g.id=kg.group_id AND g.enabled=1 JOIN group_models gm ON gm.group_id=g.id JOIN models m ON m.id=gm.model_id AND m.enabled=1 AND m.input_price_microyuan IS NOT NULL AND m.output_price_microyuan IS NOT NULL JOIN providers p ON p.id=m.provider_id AND p.enabled=1 WHERE kg.key_id=? AND m.id=?`, keyID, modelID).Scan(&allowed); err != nil {
		return KeyReservation{}, err
	}
	if allowed == 0 {
		return KeyReservation{}, errors.New("model is not authorized for key")
	}
	var usedTokens, usedAmount int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(charged_tokens+reserved_tokens),0),COALESCE(SUM(charged_amount_microyuan+reserved_amount_microyuan),0) FROM key_reservations WHERE key_id=? AND status IN ('reserved','completed')`, keyID).Scan(&usedTokens, &usedAmount); err != nil {
		return KeyReservation{}, err
	}
	if tokenLimit.Valid && (tokens > tokenLimit.Int64-usedTokens) {
		return KeyReservation{}, errors.New("key token quota exceeded")
	}
	if amountLimit.Valid && (amount > amountLimit.Int64-usedAmount) {
		return KeyReservation{}, errors.New("key amount quota exceeded")
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO key_reservations(key_id,model_id,reserved_tokens,reserved_amount_microyuan,status,created_at_utc) VALUES (?,?,?,?,?,?)`, keyID, modelID, tokens, amount, "reserved", nowUTC())
	if err != nil {
		return KeyReservation{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return KeyReservation{}, err
	}
	if err := tx.Commit(); err != nil {
		return KeyReservation{}, err
	}
	return KeyReservation{ID: id, KeyID: keyID, ModelID: modelID, ReservedTokens: tokens, ReservedAmountMicroyuan: amount, Status: "reserved"}, nil
}

func (s *Store) SettleKey(ctx context.Context, reservationID, tokens, amount int64, status string) error {
	s.reservationMu.Lock()
	defer s.reservationMu.Unlock()
	if reservationID <= 0 || tokens < 0 || amount < 0 {
		return errors.New("invalid settlement")
	}
	if status != "completed" && status != "failed" && status != "aborted" && status != "rejected" {
		return errors.New("invalid settlement status")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var keyID int64
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT key_id,status FROM key_reservations WHERE id=?`, reservationID).Scan(&keyID, &current); err != nil {
		return err
	}
	if current != "reserved" {
		return errors.New("reservation already settled")
	}
	chargedTokens, chargedAmount := int64(0), int64(0)
	if status == "completed" {
		chargedTokens, chargedAmount = tokens, amount
	}
	result, err := tx.ExecContext(ctx, `UPDATE key_reservations SET reserved_tokens=0,reserved_amount_microyuan=0,charged_tokens=?,charged_amount_microyuan=?,status=?,finished_at_utc=? WHERE id=? AND status='reserved'`, chargedTokens, chargedAmount, status, nowUTC(), reservationID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return errors.New("reservation already settled")
	}
	var totalTokens, totalAmount int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(charged_tokens),0),COALESCE(SUM(charged_amount_microyuan),0) FROM key_reservations WHERE key_id=? AND status='completed'`, keyID).Scan(&totalTokens, &totalAmount); err != nil {
		return err
	}
	var tokenLimit, amountLimit sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT token_limit,amount_limit_microyuan FROM client_keys WHERE id=?`, keyID).Scan(&tokenLimit, &amountLimit); err != nil {
		return err
	}
	if (tokenLimit.Valid && totalTokens >= tokenLimit.Int64) || (amountLimit.Valid && totalAmount >= amountLimit.Int64) {
		if _, err := tx.ExecContext(ctx, `UPDATE client_keys SET enabled=0,disabled_reason='quota_exhausted',updated_at_utc=? WHERE id=?`, nowUTC(), keyID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
