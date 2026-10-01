package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"botech/internal/domain"
	"botech/internal/storage"
)

// Ключи настроек в таблице settings.
const (
	KeyBackupTime = "backup_time"      // время ежедневного бэкапа, например "03:00" или "off"
	KeyLastBackup = "last_backup_date" // служебное: дата последнего успешного бэкапа
	KeyBackupFail = "last_backup_fail" // служебное: дата, когда админа уже предупредили об ошибке
)

const defaultBackup = "03:00"

// BackupSettings расписание бэкапа.
type BackupSettings struct {
	Enabled bool
	Hour    int
	Minute  int
}

// ParseClock разбирает время "03:00" или "off".
func ParseClock(s string) (on bool, hour, minute int, err error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "off" || s == "выкл" || s == "-" {
		return false, 0, 0, nil
	}
	a, b, ok := strings.Cut(s, ":")
	if !ok {
		return false, 0, 0, fmt.Errorf("формат: 03:00 или off")
	}
	hour, e1 := strconv.Atoi(a)
	minute, e2 := strconv.Atoi(b)
	if e1 != nil || e2 != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return false, 0, 0, fmt.Errorf("время вида 03:00")
	}
	return true, hour, minute, nil
}

// InQuiet попадает ли момент now (в нужном часовом поясе) в тихие часы. Окно может переходить через полночь.
func InQuiet(now time.Time, from, to int) bool {
	h := now.Hour()
	if from < to {
		return h >= from && h < to
	}
	return h >= from || h < to // например 22-9
}

// Settings настройки приложения, редактируемые админом.
type Settings struct {
	store storage.Store
	now   func() time.Time
}

// NewSettings создаёт сервис настроек.
func NewSettings(store storage.Store) *Settings { return &Settings{store: store, now: time.Now} }

func (s *Settings) raw(ctx context.Context, key, def string) (string, error) {
	v, ok, err := s.store.Repos().Settings.Get(ctx, key)
	if err != nil {
		return "", err
	}
	if !ok {
		return def, nil
	}
	return v, nil
}

// Backup расписание бэкапа.
func (s *Settings) Backup(ctx context.Context) (BackupSettings, error) {
	v, err := s.raw(ctx, KeyBackupTime, defaultBackup)
	if err != nil {
		return BackupSettings{}, err
	}
	on, h, m, perr := ParseClock(v)
	if perr != nil {
		on, h, m, _ = ParseClock(defaultBackup)
	}
	return BackupSettings{Enabled: on, Hour: h, Minute: m}, nil
}

// Value служебное чтение значения (например, даты последнего бэкапа).
func (s *Settings) Value(ctx context.Context, key string) (string, error) { return s.raw(ctx, key, "") }

// SetInternal записывает служебное значение без проверки и без записи в журнал админов.
func (s *Settings) SetInternal(ctx context.Context, key, value string) error {
	return s.store.Repos().Settings.Set(ctx, key, value, s.now().UTC())
}

// Set проверяет и сохраняет значение настройки от имени админа. Возвращает сохранённое (нормализованное) значение.
func (s *Settings) Set(ctx context.Context, actor int64, key, raw string) (string, error) {
	var value string
	switch key {
	case KeyBackupTime:
		on, h, m, err := ParseClock(raw)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrForbidden, err)
		}
		value = "off"
		if on {
			value = fmt.Sprintf("%02d:%02d", h, m)
		}
	default:
		return "", fmt.Errorf("%w: неизвестная настройка", ErrForbidden)
	}
	err := s.store.WithTx(ctx, func(r storage.Repos) error {
		now := s.now().UTC()
		if err := r.Settings.Set(ctx, key, value, now); err != nil {
			return err
		}
		return r.Audit.Add(ctx, &domain.AuditEntry{
			AdminID: actor, Action: "settings.set", Entity: "settings", EntityID: key, Details: value, At: now,
		})
	})
	return value, err
}
