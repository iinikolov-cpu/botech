package sqlite

import (
	"context"
	"time"

	"botech/internal/domain"
)

type auditRepo struct{ q dbtx }

func (r *auditRepo) Add(ctx context.Context, e *domain.AuditEntry) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO admin_audit (admin_id, action, entity, entity_id, details, at) VALUES (?,?,?,?,?,?)`,
		e.AdminID, e.Action, e.Entity, e.EntityID, e.Details, e.At.Unix())
	return err
}

func (r *auditRepo) List(ctx context.Context, limit int) ([]*domain.AuditEntry, error) {
	rows, err := r.q.QueryContext(ctx,
		`SELECT id, admin_id, action, entity, entity_id, details, at FROM admin_audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.AuditEntry
	for rows.Next() {
		var (
			e  domain.AuditEntry
			at int64
		)
		if err := rows.Scan(&e.ID, &e.AdminID, &e.Action, &e.Entity, &e.EntityID, &e.Details, &at); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0).UTC()
		out = append(out, &e)
	}
	return out, rows.Err()
}
