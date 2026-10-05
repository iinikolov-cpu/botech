package sqlite

import (
	"context"
	"strings"

	"botech/internal/domain"
	"botech/internal/storage"
)

type statsRepo struct{ q dbtx }

// Export выбирает задания с последней версией отчёта, компенсацией и промокодом.
func (r *statsRepo) Export(ctx context.Context, f storage.ExportFilter) ([]storage.ExportRow, error) {
	var (
		where []string
		args  []any
	)
	if !f.From.IsZero() {
		where = append(where, "t.created_at >= ?")
		args = append(args, f.From.Unix())
	}
	if !f.To.IsZero() {
		where = append(where, "t.created_at < ?")
		args = append(args, f.To.Unix())
	}
	if f.ScenarioID != 0 {
		where = append(where, "t.scenario_id = ?")
		args = append(args, f.ScenarioID)
	}
	if f.Operator != "" {
		where = append(where, "s.operator = ?")
		args = append(args, f.Operator)
	}
	cond := ""
	if len(where) > 0 {
		cond = "WHERE " + strings.Join(where, " AND ")
	}
	rows, err := r.q.QueryContext(ctx, `
SELECT t.id, t.user_id, u.first_name, u.username,
       t.scenario_id, s.key, s.title, s.operator, sv.version, t.status,
       t.created_at, t.sent_at, t.accepted_at, t.due_at, t.declined_at, t.reported_at, t.reviewed_at,
       COALESCE(r.id, 0), COALESCE(r.revision, 0), COALESCE(r.late, 0), COALESCE(r.decision, ''),
       COALESCE(c.status, ''), COALESCE(c.amount, 0), COALESCE(c.paid_at, 0),
       COALESCE(pa.code, ''),
       (SELECT COUNT(*) FROM task_events e WHERE e.task_id = t.id AND e.kind = 'reminder')
  FROM tasks t
  JOIN users u ON u.tg_id = t.user_id
  JOIN scenarios s ON s.id = t.scenario_id
  JOIN scenario_versions sv ON sv.id = t.scenario_version_id
  LEFT JOIN reports r ON r.task_id = t.id
       AND r.revision = (SELECT MAX(revision) FROM reports WHERE task_id = t.id)
  LEFT JOIN compensations c ON c.task_id = t.id
  LEFT JOIN promo_assignments pa ON pa.task_id = t.id
  `+cond+` ORDER BY t.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		out       []storage.ExportRow
		reportIdx = map[int64]int{} // id отчёта -> индекс в out
	)
	for rows.Next() {
		var (
			x                                                                          storage.ExportRow
			created, sent, accepted, due, declined, reported, reviewed, paid, reportID int64
			late                                                                       int
		)
		if err := rows.Scan(&x.TaskID, &x.UserID, &x.UserName, &x.Username,
			&x.ScenarioID, &x.Key, &x.Title, &x.Operator, &x.Version, &x.Status,
			&created, &sent, &accepted, &due, &declined, &reported, &reviewed,
			&reportID, &x.Revision, &late, &x.Decision,
			&x.CompStatus, &x.CompAmount, &paid, &x.PromoCode, &x.Reminders); err != nil {
			return nil, err
		}
		x.CreatedAt, x.SentAt, x.AcceptedAt, x.DueAt = ts(created), ts(sent), ts(accepted), ts(due)
		x.DeclinedAt, x.ReportedAt, x.ReviewedAt, x.CompPaidAt = ts(declined), ts(reported), ts(reviewed), ts(paid)
		x.Late = late != 0
		if reportID != 0 {
			reportIdx[reportID] = len(out)
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(reportIdx) == 0 {
		return out, nil
	}

	// Ответы последних версий отчётов: только по выбранным отчётам, пачками (лимит параметров SQLite).
	ids := make([]int64, 0, len(reportIdx))
	for id := range reportIdx {
		ids = append(ids, id)
	}
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		end := min(start+chunk, len(ids))
		part := ids[start:end]
		args := make([]any, len(part))
		for i, id := range part {
			args[i] = id
		}
		if err := r.loadAnswers(ctx, out, reportIdx, args); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// loadAnswers дописывает в rows ответы отчётов с указанными id.
func (r *statsRepo) loadAnswers(ctx context.Context, out []storage.ExportRow, reportIdx map[int64]int, ids []any) error {
	arows, err := r.q.QueryContext(ctx,
		`SELECT report_id, question_key, type, value, file_id, file_unique_id, skipped FROM report_answers
		  WHERE report_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`) ORDER BY id`, ids...)
	if err != nil {
		return err
	}
	defer arows.Close()
	for arows.Next() {
		var (
			rid     int64
			a       domain.Answer
			skipped int
		)
		if err := arows.Scan(&rid, &a.Key, &a.Type, &a.Value, &a.FileID, &a.FileUniqueID, &skipped); err != nil {
			return err
		}
		if i, ok := reportIdx[rid]; ok {
			a.Skipped = skipped != 0
			out[i].Answers = append(out[i].Answers, a)
		}
	}
	return arows.Err()
}
