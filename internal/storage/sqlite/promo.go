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

const assignCols = `id, promo_id, code, task_id, user_id, issued_at, finished_at, outcome`

func scanAssign(sc interface{ Scan(...any) error }) (*domain.PromoAssignment, error) {
	var (
		a        domain.PromoAssignment
		iss, fin int64
		out      string
	)
	if err := sc.Scan(&a.ID, &a.PromoID, &a.Code, &a.TaskID, &a.UserID, &iss, &fin, &out); err != nil {
		return nil, err
	}
	a.IssuedAt, a.FinishedAt, a.Outcome = ts(iss), ts(fin), domain.PromoOutcome(out)
	return &a, nil
}

func (r *promoRepo) ByTask(ctx context.Context, taskID int64) (*domain.PromoAssignment, error) {
	a, err := scanAssign(r.q.QueryRowContext(ctx, `SELECT `+assignCols+` FROM promo_assignments WHERE task_id = ?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	return a, err
}

// Issue закрепляет за заданием свободный код. Выбор кода и его закрепление делает один UPDATE
// с условием active_task_id = 0, поэтому один код не достанется двум заданиям. Метод состоит
// из двух запросов (закрепление и запись в историю), поэтому вызывается только в транзакции.
func (r *promoRepo) Issue(ctx context.Context, taskID, userID int64, maxUses int, at time.Time) (*domain.PromoAssignment, error) {
	if a, err := r.ByTask(ctx, taskID); err == nil {
		return a, nil // уже выдан ранее
	} else if !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	var promoID int64
	var code string
	err := r.q.QueryRowContext(ctx,
		`UPDATE promo_codes SET active_task_id = ?
		  WHERE id = (SELECT id FROM promo_codes WHERE active_task_id = 0 AND used_count < ? ORDER BY id LIMIT 1)
		    AND active_task_id = 0
		  RETURNING id, code`, taskID, maxUses).Scan(&promoID, &code)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a := &domain.PromoAssignment{PromoID: promoID, Code: code, TaskID: taskID, UserID: userID, IssuedAt: at, Outcome: domain.PromoActive}
	err = r.q.QueryRowContext(ctx,
		`INSERT INTO promo_assignments (promo_id, code, task_id, user_id, issued_at) VALUES (?,?,?,?,?) RETURNING id`,
		promoID, code, taskID, userID, at.Unix()).Scan(&a.ID)
	if err != nil {
		return nil, err // в транзакции закрепление откатится вместе с ошибкой
	}
	return a, nil
}

func (r *promoRepo) Release(ctx context.Context, taskID int64, used bool, at time.Time) (bool, error) {
	outcome := domain.PromoReleased
	if used {
		outcome = domain.PromoUsed
	}
	var promoID int64
	err := r.q.QueryRowContext(ctx,
		`UPDATE promo_assignments SET outcome = ?, finished_at = ? WHERE task_id = ? AND outcome = 'active' RETURNING promo_id`,
		string(outcome), at.Unix(), taskID).Scan(&promoID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	inc := 0
	if used {
		inc = 1
	}
	// Кода уже может не быть в пуле (удалён), это не ошибка: история сохранена.
	_, err = r.q.ExecContext(ctx,
		`UPDATE promo_codes SET active_task_id = 0, used_count = used_count + ? WHERE id = ? AND active_task_id = ?`,
		inc, promoID, taskID)
	return err == nil, err
}

func (r *promoRepo) Stats(ctx context.Context, maxUses int) (domain.PromoStats, error) {
	var s domain.PromoStats
	err := r.q.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN active_task_id = 0 AND used_count < ?1 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN active_task_id != 0 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN active_task_id = 0 AND used_count >= ?1 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN used_count < ?1 THEN 1 ELSE 0 END), 0)
		   FROM promo_codes`, maxUses).Scan(&s.Total, &s.Available, &s.Busy, &s.Exhausted, &s.WithUsesLeft)
	return s, err
}

func (r *promoRepo) List(ctx context.Context, limit, offset int) ([]storage.PromoRow, int, error) {
	var total int
	if err := r.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM promo_codes`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.q.QueryContext(ctx,
		`SELECT code, used_count, active_task_id FROM promo_codes ORDER BY id LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []storage.PromoRow
	for rows.Next() {
		var p storage.PromoRow
		if err := rows.Scan(&p.Code, &p.UsedCount, &p.ActiveTaskID); err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	return out, total, rows.Err()
}

func (r *promoRepo) DeleteFree(ctx context.Context, codes []string) (int, error) {
	total := 0
	for _, c := range codes {
		// Условие active_task_id = 0 в самом запросе: занятый код удалить нельзя, даже если он в списке.
		res, err := r.q.ExecContext(ctx, `DELETE FROM promo_codes WHERE code = ? AND active_task_id = 0`, c)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	return total, nil
}

func (r *promoRepo) DeleteAllFree(ctx context.Context) (int, error) {
	res, err := r.q.ExecContext(ctx, `DELETE FROM promo_codes WHERE active_task_id = 0`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
