// Пакет service содержит бизнес-логику. Он ничего не знает про Telegram и SQL.
package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"botech/internal/domain"
	"botech/internal/storage"
)

// Ошибки бизнес-правил (показываются админу понятным текстом).
var (
	ErrInvalidInvite = errors.New("инвайт недействителен")
	ErrForbidden     = errors.New("действие запрещено")
	ErrNotFound      = errors.New("пользователь не найден")
)

// inviteAlphabet: без похожих символов (0/O, 1/I), код удобно читать и вводить.
const inviteAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// InviteCodeLen длина кода: 32^10 ≈ 10^15 вариантов, перебор нереалистичен.
const InviteCodeLen = 10

// Profile данные пользователя из Telegram.
type Profile struct {
	TgID      int64
	FirstName string
	Username  string
}

// Access управляет доступом: пользователи, инвайты, админы, аудит.
type Access struct {
	store      storage.Store
	firstAdmin int64
	now        func() time.Time
}

// NewAccess создаёт сервис. firstAdmin нельзя разжаловать или заблокировать.
func NewAccess(store storage.Store, firstAdmin int64) *Access {
	return &Access{store: store, firstAdmin: firstAdmin, now: time.Now}
}

// EnsureFirstAdmin при старте гарантирует, что первый админ существует и активен.
func (a *Access) EnsureFirstAdmin(ctx context.Context) error {
	return a.store.WithTx(ctx, func(r storage.Repos) error {
		u, err := r.Users.Get(ctx, a.firstAdmin)
		if errors.Is(err, storage.ErrNotFound) {
			now := a.now().UTC()
			return r.Users.Create(ctx, &domain.User{
				TgID: a.firstAdmin, Role: domain.RoleAdmin, Status: domain.StatusActive,
				Lang: "ru", CreatedAt: now, UpdatedAt: now,
			})
		}
		if err != nil {
			return err
		}
		if u.Role == domain.RoleAdmin && u.Status == domain.StatusActive {
			return nil
		}
		u.Role, u.Status, u.UpdatedAt = domain.RoleAdmin, domain.StatusActive, a.now().UTC()
		return r.Users.Update(ctx, u)
	})
}

// Lookup возвращает пользователя или nil, если он неизвестен.
func (a *Access) Lookup(ctx context.Context, id int64) (*domain.User, error) {
	u, err := a.store.Repos().Users.Get(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	return u, err
}

// TouchProfile обновляет имя и username, если они изменились.
func (a *Access) TouchProfile(ctx context.Context, u *domain.User, p Profile) {
	if u.FirstName == p.FirstName && u.Username == p.Username {
		return
	}
	u.FirstName, u.Username, u.UpdatedAt = p.FirstName, p.Username, a.now().UTC()
	_ = a.store.Repos().Users.Update(ctx, u) // некритично, следующая попытка будет при следующем сообщении
}

// NormalizeCode приводит введённый код к каноническому виду.
func NormalizeCode(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// LooksLikeCode быстрая проверка формата до обращения к БД.
func LooksLikeCode(s string) bool {
	s = NormalizeCode(s)
	if len(s) != InviteCodeLen {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune(inviteAlphabet, c) {
			return false
		}
	}
	return true
}

// Join регистрирует нового пользователя по инвайту со статусом «ожидает одобрения».
// Инвайт и пользователь создаются в одной транзакции.
func (a *Access) Join(ctx context.Context, p Profile, code string) (*domain.User, error) {
	code = NormalizeCode(code)
	if !LooksLikeCode(code) {
		return nil, ErrInvalidInvite
	}
	var user *domain.User
	err := a.store.WithTx(ctx, func(r storage.Repos) error {
		if _, err := r.Users.Get(ctx, p.TgID); err == nil {
			return ErrForbidden // уже зарегистрирован
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		ok, err := r.Invites.Redeem(ctx, code)
		if err != nil {
			return err
		}
		if !ok {
			return ErrInvalidInvite
		}
		now := a.now().UTC()
		user = &domain.User{
			TgID: p.TgID, Role: domain.RoleBuyer, Status: domain.StatusPending, Lang: "ru",
			FirstName: p.FirstName, Username: p.Username, InviteCode: code,
			CreatedAt: now, UpdatedAt: now,
		}
		return r.Users.Create(ctx, user)
	})
	if err != nil {
		return nil, err
	}
	return user, nil
}

// SetStatus меняет статус покупателя (одобрить, заблокировать, разблокировать).
// Админов блокировать нельзя: сначала нужно снять с них права.
func (a *Access) SetStatus(ctx context.Context, actor, target int64, status domain.UserStatus) (*domain.User, error) {
	var out *domain.User
	err := a.store.WithTx(ctx, func(r storage.Repos) error {
		u, err := r.Users.Get(ctx, target)
		if errors.Is(err, storage.ErrNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if u.Role == domain.RoleAdmin || target == actor {
			return fmt.Errorf("%w: нельзя менять статус админа или себя", ErrForbidden)
		}
		if u.Status == status {
			out = u
			return nil
		}
		old := u.Status
		u.Status, u.UpdatedAt = status, a.now().UTC()
		if err := r.Users.Update(ctx, u); err != nil {
			return err
		}
		out = u
		return a.audit(ctx, r, actor, "user.status", "user", fmt.Sprint(target), string(old)+" -> "+string(status))
	})
	return out, err
}

// AddAdmin делает пользователя админом (создаёт запись, если её ещё нет).
func (a *Access) AddAdmin(ctx context.Context, actor, target int64) error {
	return a.store.WithTx(ctx, func(r storage.Repos) error {
		now := a.now().UTC()
		u, err := r.Users.Get(ctx, target)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			if err := r.Users.Create(ctx, &domain.User{
				TgID: target, Role: domain.RoleAdmin, Status: domain.StatusActive, Lang: "ru",
				CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			u.Role, u.Status, u.UpdatedAt = domain.RoleAdmin, domain.StatusActive, now
			if err := r.Users.Update(ctx, u); err != nil {
				return err
			}
		}
		return a.audit(ctx, r, actor, "admin.add", "user", fmt.Sprint(target), "")
	})
}

// RemoveAdmin снимает права админа (пользователь становится активным покупателем).
// Первого админа (из переменной окружения) и последнего админа снять нельзя.
func (a *Access) RemoveAdmin(ctx context.Context, actor, target int64) error {
	return a.store.WithTx(ctx, func(r storage.Repos) error {
		if target == a.firstAdmin {
			return fmt.Errorf("%w: это главный админ из настроек", ErrForbidden)
		}
		u, err := r.Users.Get(ctx, target)
		if errors.Is(err, storage.ErrNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if u.Role != domain.RoleAdmin {
			return nil
		}
		n, err := r.Users.Count(ctx, domain.RoleAdmin, domain.StatusActive)
		if err != nil {
			return err
		}
		if n <= 1 {
			return fmt.Errorf("%w: нельзя снять последнего админа", ErrForbidden)
		}
		u.Role, u.UpdatedAt = domain.RoleBuyer, a.now().UTC()
		if err := r.Users.Update(ctx, u); err != nil {
			return err
		}
		return a.audit(ctx, r, actor, "admin.remove", "user", fmt.Sprint(target), "")
	})
}

// CreateInvite создаёт инвайт на maxUses человек, действующий ttl (0 = бессрочно).
func (a *Access) CreateInvite(ctx context.Context, actor int64, maxUses int, ttl time.Duration) (*domain.Invite, error) {
	if maxUses < 1 {
		return nil, fmt.Errorf("%w: число использований должно быть больше нуля", ErrForbidden)
	}
	code, err := newCode()
	if err != nil {
		return nil, err
	}
	now := a.now().UTC()
	inv := &domain.Invite{Code: code, CreatedBy: actor, MaxUses: maxUses, CreatedAt: now}
	if ttl > 0 {
		inv.ExpiresAt = now.Add(ttl)
	}
	err = a.store.WithTx(ctx, func(r storage.Repos) error {
		if err := r.Invites.Create(ctx, inv); err != nil {
			return err
		}
		return a.audit(ctx, r, actor, "invite.create", "invite", code, fmt.Sprintf("max_uses=%d", maxUses))
	})
	return inv, err
}

// RevokeInvite отзывает инвайт.
func (a *Access) RevokeInvite(ctx context.Context, actor int64, code string) error {
	return a.store.WithTx(ctx, func(r storage.Repos) error {
		if err := r.Invites.Revoke(ctx, code); err != nil {
			return err
		}
		return a.audit(ctx, r, actor, "invite.revoke", "invite", code, "")
	})
}

// ActiveInvites действующие инвайты.
func (a *Access) ActiveInvites(ctx context.Context) ([]*domain.Invite, error) {
	return a.store.Repos().Invites.ListActive(ctx, 20)
}

// SetKinds сохраняет, какие типы заданий пользователь готов делать (хотя бы один).
func (a *Access) SetKinds(ctx context.Context, userID int64, kinds []domain.TaskKind) error {
	kinds = domain.ParseKinds(domain.JoinKinds(kinds)) // убирает повторы и неизвестные значения
	if len(kinds) == 0 {
		return fmt.Errorf("%w: выберите хотя бы один тип заданий", ErrForbidden)
	}
	err := a.store.Repos().Users.SetKinds(ctx, userID, kinds, a.now().UTC())
	if errors.Is(err, storage.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// UsersForKind активные покупатели, готовые делать задания этого типа (для мастера назначения).
func (a *Access) UsersForKind(ctx context.Context, kind domain.TaskKind, limit, offset int) ([]*domain.User, int, error) {
	r := a.store.Repos()
	list, err := r.Users.ListForKind(ctx, domain.StatusActive, kind, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	total, err := r.Users.CountForKind(ctx, domain.StatusActive, kind)
	return list, total, err
}

// Users список пользователей с фильтрами и страницей.
func (a *Access) Users(ctx context.Context, role domain.Role, status domain.UserStatus, limit, offset int) ([]*domain.User, int, error) {
	r := a.store.Repos()
	list, err := r.Users.List(ctx, role, status, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	total, err := r.Users.Count(ctx, role, status)
	return list, total, err
}

// AuditLog последние действия админов.
func (a *Access) AuditLog(ctx context.Context, limit int) ([]*domain.AuditEntry, error) {
	return a.store.Repos().Audit.List(ctx, limit)
}

// ActiveAdmins нужны для рассылки уведомлений.
func (a *Access) ActiveAdmins(ctx context.Context) ([]*domain.User, error) {
	return a.store.Repos().Users.ListActiveAdmins(ctx)
}

func (a *Access) audit(ctx context.Context, r storage.Repos, admin int64, action, entity, id, details string) error {
	return r.Audit.Add(ctx, &domain.AuditEntry{
		AdminID: admin, Action: action, Entity: entity, EntityID: id, Details: details, At: a.now().UTC(),
	})
}

// newCode генерирует криптостойкий код без похожих символов.
func newCode() (string, error) {
	b := make([]byte, InviteCodeLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = inviteAlphabet[int(b[i])%len(inviteAlphabet)] // 256 % 32 == 0: без смещения
	}
	return string(b), nil
}
