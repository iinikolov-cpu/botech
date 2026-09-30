package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"botech/internal/storage"
)

type fsmRepo struct{ q dbtx }

func (r *fsmRepo) Get(ctx context.Context, userID int64) (string, string, error) {
	var state, data string
	err := r.q.QueryRowContext(ctx, `SELECT state, data FROM fsm_states WHERE user_id = ?`, userID).Scan(&state, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", storage.ErrNotFound
	}
	return state, data, err
}

func (r *fsmRepo) Set(ctx context.Context, userID int64, state, data string, at time.Time) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO fsm_states (user_id, state, data, updated_at) VALUES (?,?,?,?)
		 ON CONFLICT (user_id) DO UPDATE SET state = excluded.state, data = excluded.data, updated_at = excluded.updated_at`,
		userID, state, data, at.Unix())
	return err
}

func (r *fsmRepo) Clear(ctx context.Context, userID int64) error {
	_, err := r.q.ExecContext(ctx, `DELETE FROM fsm_states WHERE user_id = ?`, userID)
	return err
}
