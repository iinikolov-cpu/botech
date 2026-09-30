// Пакет storage описывает интерфейсы репозиториев. Бизнес-логика зависит
// только от них, поэтому переход с SQLite на PostgreSQL не затронет service.
package storage

import (
	"context"
	"errors"

	"botech/internal/domain"
)

// ErrNotFound возвращается, когда запись не найдена.
var ErrNotFound = errors.New("не найдено")

// UserRepo работа с пользователями.
type UserRepo interface {
	Get(ctx context.Context, tgID int64) (*domain.User, error) // ErrNotFound, если нет
	Create(ctx context.Context, u *domain.User) error
	Update(ctx context.Context, u *domain.User) error
	List(ctx context.Context, role domain.Role, status domain.UserStatus, limit, offset int) ([]*domain.User, error)
	Count(ctx context.Context, role domain.Role, status domain.UserStatus) (int, error)
	ListActiveAdmins(ctx context.Context) ([]*domain.User, error)
}

// InviteRepo работа с инвайтами.
type InviteRepo interface {
	Create(ctx context.Context, inv *domain.Invite) error
	Get(ctx context.Context, code string) (*domain.Invite, error)
	// Redeem атомарно увеличивает счётчик использований. false, если код
	// недействителен (нет, отозван, просрочен, лимит исчерпан).
	Redeem(ctx context.Context, code string) (bool, error)
	Revoke(ctx context.Context, code string) error
	ListActive(ctx context.Context, limit int) ([]*domain.Invite, error)
}

// AuditRepo журнал действий админов.
type AuditRepo interface {
	Add(ctx context.Context, e *domain.AuditEntry) error
	List(ctx context.Context, limit int) ([]*domain.AuditEntry, error)
}

// Repos набор всех репозиториев.
type Repos struct {
	Users   UserRepo
	Invites InviteRepo
	Audit   AuditRepo
}

// Store хранилище: репозитории + транзакции.
type Store interface {
	// Repos возвращает репозитории для работы вне транзакции.
	Repos() Repos
	// WithTx выполняет fn в одной транзакции (commit при nil, иначе rollback).
	WithTx(ctx context.Context, fn func(r Repos) error) error
	Close() error
}
