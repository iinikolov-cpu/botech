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
	KeyRemindEnabled = "reminders_enabled"
	KeyRemindAccept  = "remind_accept_hours" // через сколько часов после отправки напоминать о принятии
	KeyRemindReport  = "remind_report_hours" // через сколько часов после принятия напоминать об отчёте
	KeyEscalate      = "escalate_hours"      // через сколько часов после последнего напоминания звать админа
	KeyQuiet         = "quiet_hours"         // тихие часы, например "22-9" или "off"
	KeyBackupTime    = "backup_time"         // время ежедневного бэкапа, например "03:00" или "off"
	KeyLastBackup    = "last_backup_date"    // служебное: дата последнего успешного бэкапа
	KeyBackupFail    = "last_backup_fail"    // служебное: дата, когда админа уже предупредили об ошибке
)

// Пределы значений: защита от опечаток вроде 24000 часов.
const (
	MaxRemindHours   = 720 // 30 суток
	MaxRemindCount   = 5
	MaxEscalateHours = 168
)

// ReminderSettings действующие настройки напоминаний (с учётом значений по умолчанию).
type ReminderSettings struct {
	Enabled       bool
	Accept        []int // часы от отправки задания
	Report        []int // часы от принятия (и от возврата на доработку)
	EscalateHours int
	QuietOn       bool
	QuietFrom     int // час начала тихих часов (0-23)
	QuietTo       int // час окончания
}

// BackupSettings расписание бэкапа.
type BackupSettings struct {
	Enabled bool
	Hour    int
	Minute  int
}

// Значения по умолчанию.
var (
	defaultRemind   = "24,48"
	defaultEscalate = "24"
	defaultQuiet    = "22-9"
	defaultBackup   = "03:00"
)

// ParseHours разбирает список часов "24,48". "off" или пустое значение отключает напоминания.
// Часы должны строго возрастать: так порядок напоминаний очевиден.
func ParseHours(s string) ([]int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "off" || s == "выкл" || s == "-" || s == "0" {
		return nil, nil
	}
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' })
	if len(fields) > MaxRemindCount {
		return nil, fmt.Errorf("не больше %d напоминаний", MaxRemindCount)
	}
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 || n > MaxRemindHours {
			return nil, fmt.Errorf("%q: нужно целое число часов от 1 до %d", f, MaxRemindHours)
		}
		if len(out) > 0 && n <= out[len(out)-1] {
			return nil, fmt.Errorf("часы должны идти по возрастанию, например 24,48")
		}
		out = append(out, n)
	}
	return out, nil
}

func joinHours(h []int) string {
	if len(h) == 0 {
		return "off"
	}
	parts := make([]string, len(h))
	for i, n := range h {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// ParseQuiet разбирает тихие часы "22-9" (с 22:00 до 09:00) или "off".
func ParseQuiet(s string) (on bool, from, to int, err error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "off" || s == "выкл" || s == "-" {
		return false, 0, 0, nil
	}
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return false, 0, 0, fmt.Errorf("формат: 22-9 (с какого часа по какой) или off")
	}
	from, e1 := strconv.Atoi(strings.TrimSpace(a))
	to, e2 := strconv.Atoi(strings.TrimSpace(b))
	if e1 != nil || e2 != nil || from < 0 || from > 23 || to < 0 || to > 23 || from == to {
		return false, 0, 0, fmt.Errorf("часы от 0 до 23, начало и конец должны различаться, например 22-9")
	}
	return true, from, to, nil
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

// Reminders действующие настройки напоминаний. Испорченное в БД значение заменяется
// значением по умолчанию, чтобы планировщик не останавливался.
func (s *Settings) Reminders(ctx context.Context) (ReminderSettings, error) {
	var out ReminderSettings
	en, err := s.raw(ctx, KeyRemindEnabled, "1")
	if err != nil {
		return out, err
	}
	out.Enabled = en != "0"

	get := func(key, def string) []int {
		v, e := s.raw(ctx, key, def)
		if e != nil {
			err = e
		}
		h, perr := ParseHours(v)
		if perr != nil {
			h, _ = ParseHours(def)
		}
		return h
	}
	out.Accept = get(KeyRemindAccept, defaultRemind)
	out.Report = get(KeyRemindReport, defaultRemind)

	esc, e := s.raw(ctx, KeyEscalate, defaultEscalate)
	if e != nil {
		err = e
	}
	if n, perr := strconv.Atoi(esc); perr == nil && n >= 1 && n <= MaxEscalateHours {
		out.EscalateHours = n
	} else {
		out.EscalateHours, _ = strconv.Atoi(defaultEscalate)
	}

	q, e := s.raw(ctx, KeyQuiet, defaultQuiet)
	if e != nil {
		err = e
	}
	on, from, to, perr := ParseQuiet(q)
	if perr != nil {
		on, from, to, _ = ParseQuiet(defaultQuiet)
	}
	out.QuietOn, out.QuietFrom, out.QuietTo = on, from, to
	return out, err
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
	case KeyRemindAccept, KeyRemindReport:
		h, err := ParseHours(raw)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrForbidden, err)
		}
		value = joinHours(h)
	case KeyEscalate:
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || n < 1 || n > MaxEscalateHours {
			return "", fmt.Errorf("%w: нужно целое число часов от 1 до %d", ErrForbidden, MaxEscalateHours)
		}
		value = strconv.Itoa(n)
	case KeyQuiet:
		on, from, to, err := ParseQuiet(raw)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrForbidden, err)
		}
		value = "off"
		if on {
			value = fmt.Sprintf("%d-%d", from, to)
		}
	case KeyBackupTime:
		on, h, m, err := ParseClock(raw)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrForbidden, err)
		}
		value = "off"
		if on {
			value = fmt.Sprintf("%02d:%02d", h, m)
		}
	case KeyRemindEnabled:
		value = "1"
		if v := strings.TrimSpace(raw); v == "0" || strings.EqualFold(v, "off") {
			value = "0"
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
