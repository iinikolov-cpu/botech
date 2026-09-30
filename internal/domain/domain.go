// Пакет domain содержит сущности и константы без внешних зависимостей.
package domain

import "time"

// Role роль пользователя.
type Role string

const (
	RoleAdmin Role = "admin"
	RoleBuyer Role = "buyer"
)

// UserStatus статус доступа пользователя.
type UserStatus string

const (
	StatusPending UserStatus = "pending" // подал заявку по инвайту, ждёт одобрения
	StatusActive  UserStatus = "active"
	StatusBlocked UserStatus = "blocked"
)

// User пользователь бота (админ или тайный покупатель).
type User struct {
	TgID       int64
	Role       Role
	Status     UserStatus
	Lang       string
	FirstName  string
	Username   string
	InviteCode string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// IsAdmin true для активного админа.
func (u *User) IsAdmin() bool { return u != nil && u.Role == RoleAdmin && u.Status == StatusActive }

// Invite приглашение (код), по которому можно войти в бота.
type Invite struct {
	Code      string
	CreatedBy int64
	MaxUses   int
	UsedCount int
	ExpiresAt time.Time // нулевое время = бессрочно
	Revoked   bool
	CreatedAt time.Time
}

// AuditEntry запись журнала действий админа.
type AuditEntry struct {
	ID       int64
	AdminID  int64
	Action   string
	Entity   string
	EntityID string
	Details  string
	At       time.Time
}
