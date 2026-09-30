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

type taskRepo struct{ q dbtx }

const taskCols = `id, scenario_id, scenario_version_id, user_id, status, due_days, created_by,
	created_at, sent_at, accepted_at, due_at, declined_at, reported_at, reviewed_at`

// ts переводит unix-секунды в время; 0 = нулевое время («ещё не наступило»).
func ts(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func scanTask(sc interface{ Scan(...any) error }) (*domain.Task, error) {
	var (
		t                                  domain.Task
		st                                 string
		cr, sent, acc, due, decl, rep, rev int64
	)
	if err := sc.Scan(&t.ID, &t.ScenarioID, &t.ScenarioVersionID, &t.UserID, &st, &t.DueDays, &t.CreatedBy,
		&cr, &sent, &acc, &due, &decl, &rep, &rev); err != nil {
		return nil, err
	}
	t.Status = domain.TaskStatus(st)
	t.CreatedAt, t.SentAt, t.AcceptedAt, t.DueAt = ts(cr), ts(sent), ts(acc), ts(due)
	t.DeclinedAt, t.ReportedAt, t.ReviewedAt = ts(decl), ts(rep), ts(rev)
	return &t, nil
}

func (r *taskRepo) Create(ctx context.Context, t *domain.Task) error {
	err := r.q.QueryRowContext(ctx,
		`INSERT INTO tasks (scenario_id, scenario_version_id, user_id, status, due_days, created_by, created_at)
		 VALUES (?,?,?,?,?,?,?) RETURNING id`,
		t.ScenarioID, t.ScenarioVersionID, t.UserID, t.Status, t.DueDays, t.CreatedBy, t.CreatedAt.Unix()).Scan(&t.ID)
	if isUnique(err) {
		return storage.ErrDuplicate
	}
	return err
}

func (r *taskRepo) Get(ctx context.Context, id int64) (*domain.Task, error) {
	t, err := scanTask(r.q.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	return t, err
}

func filterSQL(f storage.TaskFilter) (string, []any) {
	cond, args := "1=1", []any{}
	if f.UserID != 0 {
		cond += " AND user_id = ?"
		args = append(args, f.UserID)
	}
	if len(f.Statuses) > 0 {
		cond += " AND status IN (?" + strings.Repeat(",?", len(f.Statuses)-1) + ")"
		for _, s := range f.Statuses {
			args = append(args, string(s))
		}
	}
	return cond, args
}

func (r *taskRepo) List(ctx context.Context, f storage.TaskFilter, limit, offset int) ([]*domain.Task, error) {
	cond, args := filterSQL(f)
	args = append(args, limit, offset)
	rows, err := r.q.QueryContext(ctx,
		`SELECT `+taskCols+` FROM tasks WHERE `+cond+` ORDER BY id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *taskRepo) Count(ctx context.Context, f storage.TaskFilter) (int, error) {
	cond, args := filterSQL(f)
	var n int
	err := r.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE `+cond, args...).Scan(&n)
	return n, err
}

// timeColumn поле времени, которое заполняется при входе в статус.
var timeColumn = map[domain.TaskStatus]string{
	domain.TaskSent:     "sent_at",
	domain.TaskAccepted: "accepted_at",
	domain.TaskDeclined: "declined_at",
	domain.TaskReported: "reported_at",
	domain.TaskReviewed: "reviewed_at",
}

// Transition: условие WHERE status = from делает смену атомарной. Если две
// кнопки нажали одновременно, сработает ровно одна, вторая получит false.
func (r *taskRepo) Transition(ctx context.Context, id int64, from, to domain.TaskStatus, at, dueAt time.Time) (bool, error) {
	set := []string{"status = ?"}
	args := []any{string(to)}
	if col, ok := timeColumn[to]; ok {
		set = append(set, col+" = ?")
		args = append(args, at.Unix())
	}
	if to == domain.TaskAccepted {
		set = append(set, "due_at = ?")
		args = append(args, unix(dueAt))
	}
	args = append(args, id, string(from))
	res, err := r.q.ExecContext(ctx,
		`UPDATE tasks SET `+strings.Join(set, ", ")+` WHERE id = ? AND status = ?`, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *taskRepo) AddEvent(ctx context.Context, e *domain.TaskEvent) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO task_events (task_id, kind, from_status, to_status, actor_id, details, at) VALUES (?,?,?,?,?,?,?)`,
		e.TaskID, e.Kind, e.FromStatus, e.ToStatus, e.ActorID, e.Details, e.At.Unix())
	return err
}

func (r *taskRepo) Events(ctx context.Context, taskID int64) ([]*domain.TaskEvent, error) {
	rows, err := r.q.QueryContext(ctx,
		`SELECT id, task_id, kind, from_status, to_status, actor_id, details, at
		   FROM task_events WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.TaskEvent
	for rows.Next() {
		var (
			e        domain.TaskEvent
			from, to string
			at       int64
		)
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Kind, &from, &to, &e.ActorID, &e.Details, &at); err != nil {
			return nil, err
		}
		e.FromStatus, e.ToStatus, e.At = domain.TaskStatus(from), domain.TaskStatus(to), time.Unix(at, 0).UTC()
		out = append(out, &e)
	}
	return out, rows.Err()
}
