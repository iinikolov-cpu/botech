package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"botech/internal/domain"
	"botech/internal/storage"
)

type promoRepo struct{ q dbtx }

const promoCols = `id, code, status, task_id, user_id, added_by, created_at, issued_at, used_at, used_by`

func scanPromo(sc interface{ Scan(...any) error }) (*domain.PromoCode, error) {
	var (
		p               domain.PromoCode
		st              string
		cr, iss, usedAt int64
	)
	if err := sc.Scan(&p.ID, &p.Code, &st, &p.TaskID, &p.UserID, &p.AddedBy, &cr, &iss, &usedAt, &p.UsedBy); err != nil {
		return nil, err
	}
	p.Status = domain.PromoStatus(st)
	p.CreatedAt, p.IssuedAt, p.UsedAt = ts(cr), ts(iss), ts(usedAt)
	return &p, nil
}

func (r *promoRepo) AddBatch(ctx context.Context, codes []string, addedBy int64, at time.Time) (int, error) {
	added := 0
	for _, c := range codes {
		res, err := r.q.ExecContext(ctx,
			`INSERT INTO promo_codes (code, added_by, created_at) VALUES (?,?,?) ON CONFLICT (code) DO NOTHING`,
			c, addedBy, at.Unix())
		if err != nil {
			return added, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	return added, nil
}

func (r *promoRepo) ByTask(ctx context.Context, taskID int64) (*domain.PromoCode, error) {
	p, err := scanPromo(r.q.QueryRowContext(ctx, `SELECT `+promoCols+` FROM promo_codes WHERE task_id = ? AND task_id != 0`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	return p, err
}

func (r *promoRepo) Get(ctx context.Context, id int64) (*domain.PromoCode, error) {
	p, err := scanPromo(r.q.QueryRowContext(ctx, `SELECT `+promoCols+` FROM promo_codes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	return p, err
}

// Issue выдаёт код одним UPDATE: выбор свободного и его закрепление за заданием происходят
// атомарно, поэтому один код не уйдёт двоим. Уникальный индекс по task_id страхует от
// двух кодов на одно задание.
func (r *promoRepo) Issue(ctx context.Context, taskID, userID int64, at time.Time) (*domain.PromoCode, error) {
	if p, err := r.ByTask(ctx, taskID); err == nil {
		return p, nil // уже выдан ранее
	} else if !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	p, err := scanPromo(r.q.QueryRowContext(ctx,
		`UPDATE promo_codes SET status = 'issued', task_id = ?, user_id = ?, issued_at = ?
		  WHERE id = (SELECT id FROM promo_codes WHERE status = 'free' ORDER BY id LIMIT 1)
		  RETURNING `+promoCols, taskID, userID, at.Unix()))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, storage.ErrNotFound
	case isUnique(err):
		// Параллельный запрос успел выдать код этому заданию: возвращаем его.
		return r.ByTask(ctx, taskID)
	}
	return p, err
}

func (r *promoRepo) Stats(ctx context.Context) (domain.PromoStats, error) {
	var s domain.PromoStats
	rows, err := r.q.QueryContext(ctx, `SELECT status, COUNT(*) FROM promo_codes GROUP BY status`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return s, err
		}
		switch domain.PromoStatus(st) {
		case domain.PromoFree:
			s.Free = n
		case domain.PromoIssued:
			s.Issued = n
		case domain.PromoUsed:
			s.Used = n
		}
	}
	return s, rows.Err()
}

func (r *promoRepo) ListIssued(ctx context.Context, used bool, limit, offset int) ([]*domain.PromoCode, int, error) {
	status := domain.PromoIssued
	if used {
		status = domain.PromoUsed
	}
	var total int
	if err := r.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM promo_codes WHERE status = ?`, string(status)).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.q.QueryContext(ctx,
		`SELECT `+promoCols+` FROM promo_codes WHERE status = ? ORDER BY issued_at DESC, id DESC LIMIT ? OFFSET ?`,
		string(status), limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*domain.PromoCode
	for rows.Next() {
		p, err := scanPromo(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	return out, total, rows.Err()
}

func (r *promoRepo) MarkUsed(ctx context.Context, id, by int64, at time.Time) (bool, error) {
	res, err := r.q.ExecContext(ctx,
		`UPDATE promo_codes SET status = 'used', used_at = ?, used_by = ? WHERE id = ? AND status = 'issued'`,
		at.Unix(), by, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
