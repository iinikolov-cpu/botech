package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"botech/internal/domain"
	"botech/internal/storage"
)

type userRepo struct{ q dbtx }

const userCols = `tg_id, role, status, lang, first_name, username, invite_code, kinds, created_at, updated_at`

func scanUser(sc interface{ Scan(...any) error }) (*domain.User, error) {
	var (
		u        domain.User
		role, st string
		kinds    string
		cr, up   int64
	)
	if err := sc.Scan(&u.TgID, &role, &st, &u.Lang, &u.FirstName, &u.Username, &u.InviteCode, &kinds, &cr, &up); err != nil {
		return nil, err
	}
	u.Role, u.Status, u.Kinds = domain.Role(role), domain.UserStatus(st), domain.ParseKinds(kinds)
	u.CreatedAt, u.UpdatedAt = time.Unix(cr, 0).UTC(), time.Unix(up, 0).UTC()
	return &u, nil
}

func (r *userRepo) Get(ctx context.Context, id int64) (*domain.User, error) {
	u, err := scanUser(r.q.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE tg_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	return u, err
}

func (r *userRepo) Create(ctx context.Context, u *domain.User) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO users (`+userCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		u.TgID, u.Role, u.Status, u.Lang, u.FirstName, u.Username, u.InviteCode, domain.JoinKinds(u.Kinds),
		u.CreatedAt.Unix(), u.UpdatedAt.Unix())
	return err
}

func (r *userRepo) Update(ctx context.Context, u *domain.User) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE users SET role=?, status=?, lang=?, first_name=?, username=?, updated_at=? WHERE tg_id=?`,
		u.Role, u.Status, u.Lang, u.FirstName, u.Username, u.UpdatedAt.Unix(), u.TgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// where собирает условие по необязательным фильтрам (пустая строка = без фильтра).
func where(role domain.Role, status domain.UserStatus) (string, []any) {
	cond, args := "1=1", []any{}
	if role != "" {
		cond += " AND role = ?"
		args = append(args, role)
	}
	if status != "" {
		cond += " AND status = ?"
		args = append(args, status)
	}
	return cond, args
}

func (r *userRepo) List(ctx context.Context, role domain.Role, status domain.UserStatus, limit, offset int) ([]*domain.User, error) {
	cond, args := where(role, status)
	args = append(args, limit, offset)
	rows, err := r.q.QueryContext(ctx,
		`SELECT `+userCols+` FROM users WHERE `+cond+` ORDER BY created_at DESC, tg_id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (r *userRepo) Count(ctx context.Context, role domain.Role, status domain.UserStatus) (int, error) {
	cond, args := where(role, status)
	var n int
	err := r.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE `+cond, args...).Scan(&n)
	return n, err
}

func (r *userRepo) SetKinds(ctx context.Context, tgID int64, kinds []domain.TaskKind, at time.Time) error {
	res, err := r.q.ExecContext(ctx, `UPDATE users SET kinds=?, updated_at=? WHERE tg_id=?`, domain.JoinKinds(kinds), at.Unix(), tgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// kindCond условие «пользователь готов делать задания этого типа» (список типов хранится через запятую).
func kindCond(status domain.UserStatus, kind domain.TaskKind) (string, []any) {
	cond, args := where(domain.RoleBuyer, status)
	cond += " AND (',' || kinds || ',') LIKE ?"
	return cond, append(args, "%,"+string(kind)+",%")
}

func (r *userRepo) ListForKind(ctx context.Context, status domain.UserStatus, kind domain.TaskKind, limit, offset int) ([]*domain.User, error) {
	cond, args := kindCond(status, kind)
	args = append(args, limit, offset)
	rows, err := r.q.QueryContext(ctx,
		`SELECT `+userCols+` FROM users WHERE `+cond+` ORDER BY created_at DESC, tg_id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (r *userRepo) CountForKind(ctx context.Context, status domain.UserStatus, kind domain.TaskKind) (int, error) {
	cond, args := kindCond(status, kind)
	var n int
	err := r.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE `+cond, args...).Scan(&n)
	return n, err
}

func (r *userRepo) ListActiveAdmins(ctx context.Context) ([]*domain.User, error) {
	return r.List(ctx, domain.RoleAdmin, domain.StatusActive, 1000, 0)
}
