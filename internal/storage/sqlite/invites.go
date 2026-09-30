package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"botech/internal/domain"
	"botech/internal/storage"
)

type inviteRepo struct{ q dbtx }

const inviteCols = `code, created_by, max_uses, used_count, expires_at, revoked, created_at`

func scanInvite(sc interface{ Scan(...any) error }) (*domain.Invite, error) {
	var (
		i           domain.Invite
		exp, rev, c int64
	)
	if err := sc.Scan(&i.Code, &i.CreatedBy, &i.MaxUses, &i.UsedCount, &exp, &rev, &c); err != nil {
		return nil, err
	}
	if exp > 0 {
		i.ExpiresAt = time.Unix(exp, 0).UTC()
	}
	i.Revoked = rev != 0
	i.CreatedAt = time.Unix(c, 0).UTC()
	return &i, nil
}

func (r *inviteRepo) Create(ctx context.Context, i *domain.Invite) error {
	var exp int64
	if !i.ExpiresAt.IsZero() {
		exp = i.ExpiresAt.Unix()
	}
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO invites (`+inviteCols+`) VALUES (?,?,?,?,?,?,?)`,
		i.Code, i.CreatedBy, i.MaxUses, i.UsedCount, exp, 0, i.CreatedAt.Unix())
	return err
}

func (r *inviteRepo) Get(ctx context.Context, code string) (*domain.Invite, error) {
	i, err := scanInvite(r.q.QueryRowContext(ctx, `SELECT `+inviteCols+` FROM invites WHERE code = ?`, code))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	return i, err
}

// Redeem одним UPDATE проверяет все условия и увеличивает счётчик:
// два одновременных входа по последнему месту не пройдут оба.
func (r *inviteRepo) Redeem(ctx context.Context, code string) (bool, error) {
	res, err := r.q.ExecContext(ctx,
		`UPDATE invites SET used_count = used_count + 1
		  WHERE code = ? AND revoked = 0 AND used_count < max_uses
		    AND (expires_at = 0 OR expires_at > ?)`,
		code, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *inviteRepo) Revoke(ctx context.Context, code string) error {
	_, err := r.q.ExecContext(ctx, `UPDATE invites SET revoked = 1 WHERE code = ?`, code)
	return err
}

func (r *inviteRepo) ListActive(ctx context.Context, limit int) ([]*domain.Invite, error) {
	rows, err := r.q.QueryContext(ctx,
		`SELECT `+inviteCols+` FROM invites
		  WHERE revoked = 0 AND used_count < max_uses AND (expires_at = 0 OR expires_at > ?)
		  ORDER BY created_at DESC LIMIT ?`, time.Now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Invite
	for rows.Next() {
		i, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
