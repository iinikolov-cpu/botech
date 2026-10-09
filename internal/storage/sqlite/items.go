package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"botech/internal/domain"
	"botech/internal/storage"
)

type itemRepo struct{ q dbtx }

const itemCols = `id, title, url, price, note, added_by, created_at, status, task_id, reserved_at, finished_at`

func scanItem(sc interface{ Scan(...any) error }) (*domain.Item, error) {
	var (
		it                 domain.Item
		st                 string
		created, res, done int64
	)
	if err := sc.Scan(&it.ID, &it.Title, &it.URL, &it.Price, &it.Note, &it.AddedBy, &created, &st, &it.TaskID, &res, &done); err != nil {
		return nil, err
	}
	it.CreatedAt, it.Status, it.ReservedAt, it.FinishedAt = ts(created), domain.ItemStatus(st), ts(res), ts(done)
	return &it, nil
}

func (r *itemRepo) AddBatch(ctx context.Context, items []*domain.Item, addedBy int64, at time.Time) (int, error) {
	added := 0
	for _, it := range items {
		res, err := r.q.ExecContext(ctx,
			`INSERT INTO items (title, url, price, note, added_by, created_at) VALUES (?,?,?,?,?,?) ON CONFLICT (url) DO NOTHING`,
			it.Title, it.URL, it.Price, it.Note, addedBy, at.Unix())
		if err != nil {
			return added, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	return added, nil
}

func (r *itemRepo) Get(ctx context.Context, id int64) (*domain.Item, error) {
	it, err := scanItem(r.q.QueryRowContext(ctx, `SELECT `+itemCols+` FROM items WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	return it, err
}

func (r *itemRepo) page(ctx context.Context, cond string, limit, offset int) ([]*domain.Item, int, error) {
	var total int
	if err := r.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM items WHERE `+cond).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.q.QueryContext(ctx,
		`SELECT `+itemCols+` FROM items WHERE `+cond+` ORDER BY id LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*domain.Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, it)
	}
	return out, total, rows.Err()
}

func (r *itemRepo) ListFree(ctx context.Context, limit, offset int) ([]*domain.Item, int, error) {
	return r.page(ctx, "status = 'free'", limit, offset)
}

func (r *itemRepo) List(ctx context.Context, limit, offset int) ([]*domain.Item, int, error) {
	return r.page(ctx, "1=1", limit, offset)
}

// Reserve закрепляет айтем одним UPDATE с условием status = 'free': два покупателя не получат один айтем.
func (r *itemRepo) Reserve(ctx context.Context, itemID, taskID int64, at time.Time) (bool, error) {
	res, err := r.q.ExecContext(ctx,
		`UPDATE items SET status = 'reserved', task_id = ?, reserved_at = ? WHERE id = ? AND status = 'free'`,
		taskID, at.Unix(), itemID)
	if isUnique(err) {
		return false, storage.ErrDuplicate // у задания уже есть айтем
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *itemRepo) ByTask(ctx context.Context, taskID int64) (*domain.Item, error) {
	if taskID <= 0 {
		return nil, storage.ErrNotFound
	}
	it, err := scanItem(r.q.QueryRowContext(ctx, `SELECT `+itemCols+` FROM items WHERE task_id = ?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	return it, err
}

func (r *itemRepo) Release(ctx context.Context, taskID int64, used bool, at time.Time) (bool, error) {
	var (
		res sql.Result
		err error
	)
	if used {
		res, err = r.q.ExecContext(ctx,
			`UPDATE items SET status = 'used', finished_at = ? WHERE task_id = ? AND status = 'reserved'`, at.Unix(), taskID)
	} else {
		res, err = r.q.ExecContext(ctx,
			`UPDATE items SET status = 'free', task_id = 0, reserved_at = 0 WHERE task_id = ? AND status = 'reserved'`, taskID)
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *itemRepo) Stats(ctx context.Context) (domain.ItemStats, error) {
	var st domain.ItemStats
	err := r.q.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(status = 'free'), 0), COALESCE(SUM(status = 'reserved'), 0), COALESCE(SUM(status = 'used'), 0) FROM items`).
		Scan(&st.Total, &st.Free, &st.Reserved, &st.Used)
	return st, err
}

func (r *itemRepo) DeleteFree(ctx context.Context, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	res, err := r.q.ExecContext(ctx,
		`DELETE FROM items WHERE status = 'free' AND id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (r *itemRepo) DeleteAllFree(ctx context.Context) (int, error) {
	res, err := r.q.ExecContext(ctx, `DELETE FROM items WHERE status = 'free'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
