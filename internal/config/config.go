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

	// PromoMaxUses: сколько раз можно использовать каждый промокод (переменная PROMO_LOW_THRESHOLD).
	PromoMaxUses int
}

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
	if v := strings.TrimSpace(os.Getenv("PROMO_LOW_THRESHOLD")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, errors.New("PROMO_LOW_THRESHOLD (сколько раз можно использовать промокод) должна быть числом от 1")
		}
		c.PromoMaxUses = n
	}
	return c, nil
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
