// Пакет config читает настройки приложения только из переменных окружения.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config содержит все настройки приложения.
type Config struct {
	BotToken     string
	FirstAdminID int64
	DBPath       string
	Location     *time.Location
	LogLevel     string

	// BackupDir папка для копий базы, BackupKeep сколько последних копий хранить.
	BackupDir  string
	BackupKeep int

	// PromoMaxUses: сколько раз можно использовать каждый промокод (переменная PROMO_MAX_USES, прежнее имя PROMO_LOW_THRESHOLD тоже работает).
	PromoMaxUses int

	// Напоминания покупателям. Интервалы в минутах от момента события: отправки задания (принятие),
	// принятия задания и возврата отчёта на доработку (отчёт). Пустой список отключает напоминания.
	RemindAcceptMinutes []int
	RemindReportMinutes []int
	// Тихие часы: в это время напоминания не отправляются, а переносятся на утро.
	QuietOn   bool
	QuietFrom int
	QuietTo   int
	// StaleMinutes: через сколько минут ожидания в списке заданий появляется отметка «давно без ответа».
	StaleMinutes int
}

// Пределы для интервалов напоминаний: защита от опечаток.
const (
	MaxRemindMinutes = 43200 // 30 суток
	MaxRemindCount   = 5
)

// Load читает и проверяет переменные окружения.
func Load() (*Config, error) {
	c := &Config{
		BotToken: strings.TrimSpace(os.Getenv("BOT_TOKEN")),
		DBPath:   getenv("DB_PATH", "data/bot.db"),
		LogLevel: getenv("LOG_LEVEL", "info"),
	}
	if c.BotToken == "" {
		return nil, errors.New("не задана переменная BOT_TOKEN")
	}

	idStr := strings.TrimSpace(os.Getenv("FIRST_ADMIN_ID"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		return nil, errors.New("переменная FIRST_ADMIN_ID должна быть числом (ваш Telegram ID)")
	}
	c.FirstAdminID = id

	tz := getenv("TZ_NAME", "Asia/Tashkent")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("неизвестный часовой пояс %q: %w", tz, err)
	}
	c.Location = loc

	c.BackupDir = getenv("BACKUP_DIR", filepath.Join(filepath.Dir(c.DBPath), "backups"))
	c.BackupKeep = 7
	if v := strings.TrimSpace(os.Getenv("BACKUP_KEEP")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 365 {
			return nil, errors.New("BACKUP_KEEP должна быть числом от 1 до 365")
		}
		c.BackupKeep = n
	}

	c.PromoMaxUses = 1
	name, v := "PROMO_MAX_USES", strings.TrimSpace(os.Getenv("PROMO_MAX_USES"))
	if v == "" { // прежнее имя переменной, оставлено для совместимости со старыми файлами .env
		name, v = "PROMO_LOW_THRESHOLD", strings.TrimSpace(os.Getenv("PROMO_LOW_THRESHOLD"))
	}
	if v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%s (сколько раз можно использовать промокод) должна быть числом от 1", name)
		}
		c.PromoMaxUses = n
	}

	var perr error
	if c.RemindAcceptMinutes, perr = ParseMinutes(getenv("REMIND_ACCEPT_MINUTES", "1440,2880")); perr != nil {
		return nil, fmt.Errorf("REMIND_ACCEPT_MINUTES: %w", perr)
	}
	if c.RemindReportMinutes, perr = ParseMinutes(getenv("REMIND_REPORT_MINUTES", "1440,2880")); perr != nil {
		return nil, fmt.Errorf("REMIND_REPORT_MINUTES: %w", perr)
	}
	if c.QuietOn, c.QuietFrom, c.QuietTo, perr = ParseQuiet(getenv("REMIND_QUIET_HOURS", "22-9")); perr != nil {
		return nil, fmt.Errorf("REMIND_QUIET_HOURS: %w", perr)
	}
	c.StaleMinutes = 1440
	if v := strings.TrimSpace(os.Getenv("STALE_MINUTES")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, errors.New("STALE_MINUTES должна быть числом минут от 1")
		}
		c.StaleMinutes = n
	}
	return c, nil
}

// ParseMinutes разбирает список минут "1440,2880" (строго по возрастанию). "off" или пустое значение
// отключает напоминания.
func ParseMinutes(s string) ([]int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "off" || s == "-" || s == "0" {
		return nil, nil
	}
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' })
	if len(fields) > MaxRemindCount {
		return nil, fmt.Errorf("не больше %d напоминаний", MaxRemindCount)
	}
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 || n > MaxRemindMinutes {
			return nil, fmt.Errorf("%q: нужно целое число минут от 1 до %d", f, MaxRemindMinutes)
		}
		if len(out) > 0 && n <= out[len(out)-1] {
			return nil, errors.New("минуты должны идти по возрастанию, например 1440,2880")
		}
		out = append(out, n)
	}
	return out, nil
}

// ParseQuiet разбирает тихие часы "22-9" (с 22:00 до 09:00) или "off".
func ParseQuiet(s string) (on bool, from, to int, err error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "off" || s == "-" {
		return false, 0, 0, nil
	}
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return false, 0, 0, errors.New("формат: 22-9 (с какого часа по какой) или off")
	}
	from, e1 := strconv.Atoi(strings.TrimSpace(a))
	to, e2 := strconv.Atoi(strings.TrimSpace(b))
	if e1 != nil || e2 != nil || from < 0 || from > 23 || to < 0 || to > 23 || from == to {
		return false, 0, 0, errors.New("часы от 0 до 23, начало и конец должны различаться, например 22-9")
	}
	return true, from, to, nil
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
