package service

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"botech/internal/domain"
	"botech/internal/storage/sqlite"
)

const firstAdmin = int64(1000)

// newTestAccess поднимает сервис на временной SQLite-базе (файл, т.к. в памяти
// у каждого соединения была бы своя БД).
func newTestAccess(t *testing.T) *Access {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	a := NewAccess(store, firstAdmin)
	if err := a.EnsureFirstAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestJoinFlow(t *testing.T) {
	ctx := context.Background()
	a := newTestAccess(t)
	inv, err := a.CreateInvite(ctx, firstAdmin, 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		user    int64
		code    string
		wantErr error
	}{
		{"мусорный код", 1, "abc", ErrInvalidInvite},
		{"несуществующий код", 1, "AAAAAAAAAA", ErrInvalidInvite},
		{"валидный код, нижний регистр", 2, " " + lower(inv.Code) + " ", nil},
		{"код израсходован", 3, inv.Code, ErrInvalidInvite},
		{"уже зарегистрирован", 2, inv.Code, ErrForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := a.Join(ctx, Profile{TgID: tc.user, FirstName: "X"}, tc.code)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ошибка: получили %v, ожидали %v", err, tc.wantErr)
			}
			if err == nil && u.Status != domain.StatusPending {
				t.Fatalf("новый пользователь должен быть pending, а не %s", u.Status)
			}
		})
	}
}

func TestJoinRevokedAndExpired(t *testing.T) {
	ctx := context.Background()
	a := newTestAccess(t)

	revoked, _ := a.CreateInvite(ctx, firstAdmin, 5, time.Hour)
	if err := a.RevokeInvite(ctx, firstAdmin, revoked.Code); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Join(ctx, Profile{TgID: 1}, revoked.Code); !errors.Is(err, ErrInvalidInvite) {
		t.Fatalf("отозванный инвайт должен не работать, err=%v", err)
	}

	expired, _ := a.CreateInvite(ctx, firstAdmin, 5, time.Hour)
	// Сдвигаем «часы» сервиса нельзя (Redeem смотрит на реальное время БД),
	// поэтому правим срок в базе напрямую.
	if _, err := a.store.(*sqlite.Store).DB().ExecContext(ctx,
		`UPDATE invites SET expires_at = ? WHERE code = ?`, time.Now().Add(-time.Minute).Unix(), expired.Code); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Join(ctx, Profile{TgID: 2}, expired.Code); !errors.Is(err, ErrInvalidInvite) {
		t.Fatalf("просроченный инвайт должен не работать, err=%v", err)
	}
}

// Инвайт на N мест при M>N одновременных входах должен пустить ровно N человек.
func TestJoinConcurrentLimit(t *testing.T) {
	ctx := context.Background()
	a := newTestAccess(t)
	const seats, tries = 3, 30
	inv, _ := a.CreateInvite(ctx, firstAdmin, seats, time.Hour)

	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < tries; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			if _, err := a.Join(ctx, Profile{TgID: id}, inv.Code); err == nil {
				ok.Add(1)
			}
		}(int64(100 + i))
	}
	wg.Wait()
	if ok.Load() != seats {
		t.Fatalf("вошло %d человек, ожидали ровно %d", ok.Load(), seats)
	}
}

func TestSetStatusRules(t *testing.T) {
	ctx := context.Background()
	a := newTestAccess(t)
	inv, _ := a.CreateInvite(ctx, firstAdmin, 5, time.Hour)
	if _, err := a.Join(ctx, Profile{TgID: 7}, inv.Code); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		actor   int64
		target  int64
		status  domain.UserStatus
		wantErr error
	}{
		{"одобрить", firstAdmin, 7, domain.StatusActive, nil},
		{"заблокировать", firstAdmin, 7, domain.StatusBlocked, nil},
		{"разблокировать", firstAdmin, 7, domain.StatusActive, nil},
		{"нельзя блокировать админа", firstAdmin, firstAdmin, domain.StatusBlocked, ErrForbidden},
		{"нет такого пользователя", firstAdmin, 999, domain.StatusActive, ErrNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := a.SetStatus(ctx, tc.actor, tc.target, tc.status)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ошибка: получили %v, ожидали %v", err, tc.wantErr)
			}
			if err == nil && u.Status != tc.status {
				t.Fatalf("статус %s, ожидали %s", u.Status, tc.status)
			}
		})
	}

	log, _ := a.AuditLog(ctx, 10)
	if len(log) != 4 { // создание инвайта + три успешные смены статуса
		t.Fatalf("в журнале %d записей, ожидали 4", len(log))
	}
}

func TestAdminManagement(t *testing.T) {
	ctx := context.Background()
	a := newTestAccess(t)

	if err := a.RemoveAdmin(ctx, firstAdmin, firstAdmin); !errors.Is(err, ErrForbidden) {
		t.Fatalf("главного админа снять нельзя, err=%v", err)
	}
	if err := a.AddAdmin(ctx, firstAdmin, 2000); err != nil {
		t.Fatal(err)
	}
	u, _ := a.Lookup(ctx, 2000)
	if !u.IsAdmin() {
		t.Fatal("пользователь 2000 должен быть админом")
	}
	if err := a.RemoveAdmin(ctx, firstAdmin, 2000); err != nil {
		t.Fatal(err)
	}
	u, _ = a.Lookup(ctx, 2000)
	if u.IsAdmin() {
		t.Fatal("права админа должны быть сняты")
	}
}

func TestEnsureFirstAdminRestores(t *testing.T) {
	ctx := context.Background()
	a := newTestAccess(t)
	// Даже если запись испортили, при старте первый админ восстанавливается.
	if _, err := a.store.(*sqlite.Store).DB().ExecContext(ctx,
		`UPDATE users SET role='buyer', status='blocked' WHERE tg_id = ?`, firstAdmin); err != nil {
		t.Fatal(err)
	}
	if err := a.EnsureFirstAdmin(ctx); err != nil {
		t.Fatal(err)
	}
	u, _ := a.Lookup(ctx, firstAdmin)
	if !u.IsAdmin() {
		t.Fatal("первый админ должен быть восстановлен")
	}
}

func TestLooksLikeCode(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"ABCDEFGH23", true},
		{"abcdefgh23", true},
		{"ABCDEFGH2", false},   // короткий
		{"ABCDEFGH234", false}, // длинный
		{"ABCDEFGH0O", false},  // запрещённые символы
		{"привет всем", false},
	}
	for _, tc := range tests {
		if got := LooksLikeCode(tc.in); got != tc.want {
			t.Errorf("LooksLikeCode(%q) = %v, ожидали %v", tc.in, got, tc.want)
		}
	}
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
