package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type settingsRepo struct{ q dbtx }

func (r *settingsRepo) Get(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := r.q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (r *settingsRepo) Set(ctx context.Context, key, value string, at time.Time) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?,?,?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, at.Unix())
	return err
}

func (r *settingsRepo) All(ctx context.Context) (map[string]string, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

type reminderRepo struct{ q dbtx }

func (r *reminderRepo) Seqs(ctx context.Context, taskID int64, kind string, baseAt time.Time) (map[int]bool, error) {
	rows, err := r.q.QueryContext(ctx,
		`SELECT seq FROM task_reminders WHERE task_id = ? AND kind = ? AND base_at = ?`, taskID, kind, baseAt.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var s int
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out[s] = true
	}
	return out, rows.Err()
}

func (r *reminderRepo) Record(ctx context.Context, taskID int64, kind string, baseAt time.Time, seq int, skipped bool, at time.Time) (bool, error) {
	res, err := r.q.ExecContext(ctx,
		`INSERT INTO task_reminders (task_id, kind, base_at, seq, sent_at, skipped) VALUES (?,?,?,?,?,?)
		 ON CONFLICT (task_id, kind, base_at, seq) DO NOTHING`,
		taskID, kind, baseAt.Unix(), seq, at.Unix(), b2i(skipped))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *reminderRepo) Sent(ctx context.Context, taskID int64, kind string, baseAt time.Time) (int, error) {
	var n int
	err := r.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task_reminders WHERE task_id = ? AND kind = ? AND base_at = ? AND seq >= 1 AND skipped = 0`,
		taskID, kind, baseAt.Unix()).Scan(&n)
	return n, err
}
