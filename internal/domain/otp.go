package domain

import "time"

// EmailOTP выданный на почту одноразовый код. Сам код не хранится: только хэш с солью.
type EmailOTP struct {
	TgID      int64
	Email     string // каноническая почта, на которую отправлен код
	Salt      string
	CodeHash  string
	ExpiresAt time.Time
	Attempts  int // сколько раз уже вводили неверный код
	SentAt    time.Time
}
