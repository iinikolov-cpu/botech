package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"botech/internal/domain"
	"botech/internal/storage"
)

type otpRepo struct{ q dbtx }

func (r *otpRepo) Put(ctx context.Context, o *domain.EmailOTP) error {
	_, err := r.q.ExecContext(ctx,
		`INSERT INTO email_otps (tg_id, email, salt, code_hash, expires_at, attempts, sent_at) VALUES (?,?,?,?,?,0,?)
		 ON CONFLICT (tg_id) DO UPDATE SET email = excluded.email, salt = excluded.salt, code_hash = excluded.code_hash,
		   expires_at = excluded.expires_at, attempts = 0, sent_at = excluded.sent_at`,
		o.TgID, o.Email, o.Salt, o.CodeHash, o.ExpiresAt.Unix(), o.SentAt.Unix())
	return err
}

func (r *otpRepo) Get(ctx context.Context, tgID int64) (*domain.EmailOTP, error) {
	var (
		o       domain.EmailOTP
		exp, at int64
	)
	err := r.q.QueryRowContext(ctx,
		`SELECT tg_id, email, salt, code_hash, expires_at, attempts, sent_at FROM email_otps WHERE tg_id = ?`, tgID).
		Scan(&o.TgID, &o.Email, &o.Salt, &o.CodeHash, &exp, &o.Attempts, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	o.ExpiresAt, o.SentAt = ts(exp), ts(at)
	return &o, nil
}

func (r *otpRepo) AddAttempt(ctx context.Context, tgID int64) (int, error) {
	var n int
	err := r.q.QueryRowContext(ctx, `UPDATE email_otps SET attempts = attempts + 1 WHERE tg_id = ? RETURNING attempts`, tgID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, storage.ErrNotFound
	}
	return n, err
}

func (r *otpRepo) Delete(ctx context.Context, tgID int64) error {
	_, err := r.q.ExecContext(ctx, `DELETE FROM email_otps WHERE tg_id = ?`, tgID)
	return err
}

func (r *otpRepo) RecordSend(ctx context.Context, tgID int64, email string, at time.Time) error {
	_, err := r.q.ExecContext(ctx, `INSERT INTO email_otp_sends (tg_id, email, at) VALUES (?,?,?)`, tgID, email, at.Unix())
	return err
}

// sendCond условие журнала: по аккаунту, по почте или все отправки.
func sendCond(tgID int64, email string, since time.Time) (string, []any) {
	cond, args := "at >= ?", []any{since.Unix()}
	if tgID != 0 {
		cond += " AND tg_id = ?"
		args = append(args, tgID)
	}
	if email != "" {
		cond += " AND email = ?"
		args = append(args, email)
	}
	return cond, args
}

func (r *otpRepo) CountSends(ctx context.Context, tgID int64, email string, since time.Time) (int, error) {
	cond, args := sendCond(tgID, email, since)
	var n int
	err := r.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM email_otp_sends WHERE `+cond, args...).Scan(&n)
	return n, err
}

func (r *otpRepo) OldestSend(ctx context.Context, tgID int64, email string, since time.Time) (time.Time, error) {
	cond, args := sendCond(tgID, email, since)
	var at sql.NullInt64
	if err := r.q.QueryRowContext(ctx, `SELECT MIN(at) FROM email_otp_sends WHERE `+cond, args...).Scan(&at); err != nil {
		return time.Time{}, err
	}
	if !at.Valid {
		return time.Time{}, nil
	}
	return ts(at.Int64), nil
}

func (r *otpRepo) Purge(ctx context.Context, now, before time.Time) error {
	if _, err := r.q.ExecContext(ctx, `DELETE FROM email_otps WHERE expires_at < ?`, now.Unix()); err != nil {
		return err
	}
	_, err := r.q.ExecContext(ctx, `DELETE FROM email_otp_sends WHERE at < ?`, before.Unix())
	return err
}
