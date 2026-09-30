package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"botech/internal/domain"
	"botech/internal/storage"
)

type reportRepo struct{ q dbtx }

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (r *reportRepo) Create(ctx context.Context, rep *domain.Report) error {
	err := r.q.QueryRowContext(ctx,
		`INSERT INTO reports (task_id, user_id, late, submitted_at) VALUES (?,?,?,?) RETURNING id`,
		rep.TaskID, rep.UserID, b2i(rep.Late), rep.SubmittedAt.Unix()).Scan(&rep.ID)
	if isUnique(err) {
		return storage.ErrDuplicate
	}
	if err != nil {
		return err
	}
	for _, a := range rep.Answers {
		if _, err := r.q.ExecContext(ctx,
			`INSERT INTO report_answers (report_id, question_key, type, value, file_id, file_unique_id, skipped)
			 VALUES (?,?,?,?,?,?,?)`,
			rep.ID, a.Key, a.Type, a.Value, a.FileID, a.FileUniqueID, b2i(a.Skipped)); err != nil {
			return err
		}
	}
	return nil
}

func (r *reportRepo) ByTask(ctx context.Context, taskID int64) (*domain.Report, error) {
	var (
		rep       domain.Report
		late, sub int64
	)
	err := r.q.QueryRowContext(ctx, `SELECT id, task_id, user_id, late, submitted_at FROM reports WHERE task_id = ?`, taskID).
		Scan(&rep.ID, &rep.TaskID, &rep.UserID, &late, &sub)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rep.Late, rep.SubmittedAt = late != 0, time.Unix(sub, 0).UTC()

	rows, err := r.q.QueryContext(ctx,
		`SELECT question_key, type, value, file_id, file_unique_id, skipped FROM report_answers WHERE report_id = ? ORDER BY id`, rep.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			a       domain.Answer
			typ     string
			skipped int64
		)
		if err := rows.Scan(&a.Key, &typ, &a.Value, &a.FileID, &a.FileUniqueID, &skipped); err != nil {
			return nil, err
		}
		a.Type, a.Skipped = domain.QuestionType(typ), skipped != 0
		rep.Answers = append(rep.Answers, a)
	}
	return &rep, rows.Err()
}

type compRepo struct{ q dbtx }

const compCols = `id, task_id, user_id, amount, receipt_file_id, receipt_unique_id, status, created_at, paid_at, paid_by`

func scanComp(sc interface{ Scan(...any) error }) (*domain.Compensation, error) {
	var (
		c          domain.Compensation
		st         string
		cr, paidAt int64
	)
	if err := sc.Scan(&c.ID, &c.TaskID, &c.UserID, &c.Amount, &c.ReceiptFileID, &c.ReceiptUniqueID, &st, &cr, &paidAt, &c.PaidBy); err != nil {
		return nil, err
	}
	c.Status, c.CreatedAt, c.PaidAt = domain.CompStatus(st), ts(cr), ts(paidAt)
	return &c, nil
}

func (r *compRepo) Create(ctx context.Context, c *domain.Compensation) error {
	err := r.q.QueryRowContext(ctx,
		`INSERT INTO compensations (task_id, user_id, amount, receipt_file_id, receipt_unique_id, status, created_at)
		 VALUES (?,?,?,?,?,'pending',?) RETURNING id`,
		c.TaskID, c.UserID, c.Amount, c.ReceiptFileID, c.ReceiptUniqueID, c.CreatedAt.Unix()).Scan(&c.ID)
	if isUnique(err) {
		return storage.ErrDuplicate
	}
	return err
}

func (r *compRepo) one(ctx context.Context, where string, arg any) (*domain.Compensation, error) {
	c, err := scanComp(r.q.QueryRowContext(ctx, `SELECT `+compCols+` FROM compensations WHERE `+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	return c, err
}

func (r *compRepo) Get(ctx context.Context, id int64) (*domain.Compensation, error) {
	return r.one(ctx, "id = ?", id)
}

func (r *compRepo) ByTask(ctx context.Context, taskID int64) (*domain.Compensation, error) {
	return r.one(ctx, "task_id = ?", taskID)
}

func (r *compRepo) List(ctx context.Context, status domain.CompStatus, limit, offset int) ([]*domain.Compensation, int, error) {
	var total int
	if err := r.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM compensations WHERE status = ?`, string(status)).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.q.QueryContext(ctx,
		`SELECT `+compCols+` FROM compensations WHERE status = ? ORDER BY id DESC LIMIT ? OFFSET ?`, string(status), limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*domain.Compensation
	for rows.Next() {
		c, err := scanComp(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

func (r *compRepo) SumByStatus(ctx context.Context, status domain.CompStatus) (int64, error) {
	var sum int64
	err := r.q.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount), 0) FROM compensations WHERE status = ?`, string(status)).Scan(&sum)
	return sum, err
}

func (r *compRepo) MarkPaid(ctx context.Context, id, by int64, at time.Time) (bool, error) {
	res, err := r.q.ExecContext(ctx,
		`UPDATE compensations SET status = 'paid', paid_at = ?, paid_by = ? WHERE id = ? AND status = 'pending'`,
		at.Unix(), by, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *compRepo) FindByReceipt(ctx context.Context, uniqueID string, exceptTaskID int64) (int64, bool, error) {
	if uniqueID == "" {
		return 0, false, nil
	}
	var id int64
	err := r.q.QueryRowContext(ctx,
		`SELECT task_id FROM compensations WHERE receipt_unique_id = ? AND task_id != ? ORDER BY id LIMIT 1`,
		uniqueID, exceptTaskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}
