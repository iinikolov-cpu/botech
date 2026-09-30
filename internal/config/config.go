// Пакет config читает настройки приложения только из переменных окружения.
package config

import (
	"errors"
	"fmt"
	"os"
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

	// PromoLowThreshold: при таком остатке промокодов в пуле админы получают предупреждение.
	PromoLowThreshold int
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

	c.PromoLowThreshold = 5
	if v := strings.TrimSpace(os.Getenv("PROMO_LOW_THRESHOLD")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, errors.New("PROMO_LOW_THRESHOLD должна быть неотрицательным числом")
		}
		c.PromoLowThreshold = n
	}
	return c, nil
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
