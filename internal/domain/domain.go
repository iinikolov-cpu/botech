// Пакет domain содержит сущности и константы без внешних зависимостей.
package domain

import (
	"strings"
	"time"
)

// Role роль пользователя.
type Role string

const (
	RoleAdmin Role = "admin"
	RoleBuyer Role = "buyer"
)

// TaskKind тип задания: чем занимается тайный участник.
type TaskKind string

const (
	KindBuyer  TaskKind = "buyer"  // тайный покупатель: айтем, промокод на доставку, компенсация
	KindSeller TaskKind = "seller" // тайный продавец: без айтема, промокода и компенсации
)

// AllKinds все типы заданий в порядке показа.
var AllKinds = []TaskKind{KindBuyer, KindSeller}

// Valid true для известных типов.
func (k TaskKind) Valid() bool { return k == KindBuyer || k == KindSeller }

// Title название типа для показа админу.
func (k TaskKind) Title() string {
	if k == KindSeller {
		return "продавец"
	}
	return "покупатель"
}

// ParseKinds разбирает набор типов из строки "buyer,seller"; неизвестные значения отбрасываются.
func ParseKinds(s string) []TaskKind {
	var out []TaskKind
	for _, f := range strings.Split(s, ",") {
		k := TaskKind(strings.TrimSpace(f))
		if k.Valid() && !hasKind(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// JoinKinds записывает набор типов строкой в порядке AllKinds.
func JoinKinds(kinds []TaskKind) string {
	var parts []string
	for _, k := range AllKinds {
		if hasKind(kinds, k) {
			parts = append(parts, string(k))
		}
	}
	return strings.Join(parts, ",")
}

func hasKind(list []TaskKind, k TaskKind) bool {
	for _, x := range list {
		if x == k {
			return true
		}
	}
	return false
}

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
	Kinds      []TaskKind // какие типы заданий готов делать; пусто, пока не выбрал
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// CanDo готов ли пользователь делать задания этого типа.
func (u *User) CanDo(k TaskKind) bool { return u != nil && hasKind(u.Kinds, k) }

// KindsTitle типы заданий пользователя для показа: «покупатель, продавец» или «не выбраны».
func (u *User) KindsTitle() string {
	if len(u.Kinds) == 0 {
		return "не выбраны"
	}
	var parts []string
	for _, k := range AllKinds {
		if hasKind(u.Kinds, k) {
			parts = append(parts, k.Title())
		}
	}
	return strings.Join(parts, ", ")
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
