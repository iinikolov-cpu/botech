package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"botech/internal/domain"
	"botech/internal/mailer"
	"botech/internal/storage"
)

// Ограничения входа по почте.
const (
	OTPLength        = 6                // цифр в коде
	MaxOTPAttempts   = 5                // неверных вводов, после которых код сгорает
	ResendCooldown   = 60 * time.Second // пауза перед повторной отправкой
	SendWindow       = time.Hour        // окно для лимитов отправки
	MaxSendsPerTg    = 5                // писем на один Telegram-аккаунт за окно
	MaxSendsPerEmail = 5                // писем на одну почту за окно
	MaxSendsTotal    = 300              // всего писем за окно (защита почтового сервера от рассылки)
	maxEmailLen      = 254
)

// Ошибки входа по почте. Тексты показываются пользователю.
var (
	ErrBadEmail        = errors.New("укажите рабочую почту на разрешённом домене")
	ErrEmailTaken      = errors.New("эта почта уже привязана к другому аккаунту Telegram, обратитесь к администратору")
	ErrNoPendingCode   = errors.New("код не запрашивался или уже недействителен: пришлите почту ещё раз")
	ErrCodeExpired     = errors.New("срок действия кода истёк: пришлите почту ещё раз и получите новый код")
	ErrCodeInvalid     = errors.New("неверный код")
	ErrTooManyAttempts = errors.New("слишком много неверных вводов: код аннулирован, запросите новый")
	ErrMailFailed      = errors.New("не удалось отправить письмо, попробуйте позже или обратитесь к администратору")
)

// TooManyRequestsError превышен лимит запросов кода; Wait сколько подождать.
type TooManyRequestsError struct{ Wait time.Duration }

func (e *TooManyRequestsError) Error() string {
	mins := int(e.Wait.Minutes()) + 1
	if e.Wait < time.Minute {
		return fmt.Sprintf("код уже отправлен, повторить можно через %d сек.", int(e.Wait.Seconds())+1)
	}
	return fmt.Sprintf("слишком много запросов кода, попробуйте через %d мин.", mins)
}

// EmailAuth вход по почте сотрудника: проверка домена, одноразовый код в письме, выдача доступа.
type EmailAuth struct {
	store   storage.Store
	mail    mailer.Mailer
	domains []string
	ttl     time.Duration
	now     func() time.Time
	rnd     func() (string, error) // генератор кода (в тестах подменяется)
}

// NewEmailAuth создаёт сервис. domains: разрешённые домены, ttl: срок действия кода.
func NewEmailAuth(store storage.Store, m mailer.Mailer, domains []string, ttl time.Duration) *EmailAuth {
	return &EmailAuth{store: store, mail: m, domains: domains, ttl: ttl, now: time.Now, rnd: randomCode}
}

// TTL срок действия кода.
func (e *EmailAuth) TTL() time.Duration { return e.ttl }

// Domains разрешённые домены.
func (e *EmailAuth) Domains() []string { return e.domains }

var localRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._'-]*$`)

// NormalizeEmail приводит почту к каноническому виду и проверяет домен.
// canonical: строчными и без метки «+что-то» (иначе один сотрудник заводил бы много аккаунтов через
// a+1@, a+2@). deliver: адрес, на который отправляется письмо (как ввёл пользователь, но строчными).
func NormalizeEmail(raw string, domains []string) (canonical, deliver string, err error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.Trim(s, "<>")
	if s == "" || len(s) > maxEmailLen || strings.ContainsAny(s, " \t\r\n,;\"") {
		return "", "", ErrBadEmail
	}
	local, dom, ok := strings.Cut(s, "@")
	if !ok || strings.Contains(dom, "@") || local == "" || len(local) > 64 {
		return "", "", ErrBadEmail
	}
	allowed := false
	for _, d := range domains {
		allowed = allowed || dom == d
	}
	if !allowed {
		return "", "", ErrBadEmail
	}
	base, _, _ := strings.Cut(local, "+")
	if !localRe.MatchString(base) || strings.Contains(local, "..") {
		return "", "", ErrBadEmail
	}
	return base + "@" + dom, local + "@" + dom, nil
}

// randomCode случайный числовой код из OTPLength цифр (crypto/rand, без смещения).
func randomCode() (string, error) {
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(OTPLength), nil)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", OTPLength, n), nil
}

func newSalt() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hashCode хэш кода с солью и привязкой к аккаунту и почте: код нельзя использовать ни для чего другого.
func hashCode(salt, code string, tgID int64, email string) string {
	m := hmac.New(sha256.New, []byte(salt))
	fmt.Fprintf(m, "%s|%d|%s", code, tgID, email)
	return hex.EncodeToString(m.Sum(nil))
}

// MaskEmail прячет почту для логов: «iv***@dila-team.com».
func MaskEmail(email string) string {
	local, dom, ok := strings.Cut(email, "@")
	if !ok {
		return "***"
	}
	if len(local) > 2 {
		local = local[:2]
	}
	return local + "***@" + dom
}

// HasPending есть ли у аккаунта действующий (не истёкший) код.
func (e *EmailAuth) HasPending(ctx context.Context, tgID int64) bool {
	o, err := e.store.Repos().OTP.Get(ctx, tgID)
	return err == nil && e.now().Before(o.ExpiresAt)
}

// owner проверяет, не занята ли почта другим аккаунтом. Если владелец заблокирован, почта освобождается:
// так сотрудник может войти с нового аккаунта Telegram после блокировки старого админом.
func owner(ctx context.Context, r storage.Repos, email string, tgID int64, now time.Time) error {
	u, err := r.Users.ByEmail(ctx, email)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if u.TgID == tgID {
		return nil
	}
	if u.Status == domain.StatusBlocked {
		return r.Users.ClearEmail(ctx, u.TgID, now)
	}
	return ErrEmailTaken
}

// RequestCode проверяет почту и отправляет на неё код. Возвращает каноническую почту.
// Пользователь не должен быть уже зарегистрирован (это проверяет вызывающий через Lookup).
func (e *EmailAuth) RequestCode(ctx context.Context, p Profile, raw string) (string, error) {
	canonical, deliver, err := NormalizeEmail(raw, e.domains)
	if err != nil {
		return "", err
	}
	now := e.now().UTC()
	code, err := e.rnd()
	if err != nil {
		return "", err
	}
	salt, err := newSalt()
	if err != nil {
		return "", err
	}
	otp := &domain.EmailOTP{
		TgID: p.TgID, Email: canonical, Salt: salt, CodeHash: hashCode(salt, code, p.TgID, canonical),
		ExpiresAt: now.Add(e.ttl), SentAt: now,
	}

	err = e.store.WithTx(ctx, func(r storage.Repos) error {
		_ = r.OTP.Purge(ctx, now, now.Add(-24*time.Hour)) // чистка устаревшего, ошибка не важна
		if err := owner(ctx, r, canonical, p.TgID, now); err != nil {
			return err
		}
		// Пауза между отправками и лимиты по аккаунту, по почте и общий.
		if prev, err := r.OTP.Get(ctx, p.TgID); err == nil {
			if wait := ResendCooldown - now.Sub(prev.SentAt); wait > 0 {
				return &TooManyRequestsError{Wait: wait}
			}
		}
		since := now.Add(-SendWindow)
		for _, l := range []struct {
			tg    int64
			email string
			max   int
		}{{p.TgID, "", MaxSendsPerTg}, {0, canonical, MaxSendsPerEmail}, {0, "", MaxSendsTotal}} {
			n, err := r.OTP.CountSends(ctx, l.tg, l.email, since)
			if err != nil {
				return err
			}
			if n >= l.max {
				oldest, err := r.OTP.OldestSend(ctx, l.tg, l.email, since)
				if err != nil {
					return err
				}
				return &TooManyRequestsError{Wait: oldest.Add(SendWindow).Sub(now)}
			}
		}
		if err := r.OTP.Put(ctx, otp); err != nil {
			return err
		}
		return r.OTP.RecordSend(ctx, p.TgID, canonical, now)
	})
	if err != nil {
		return "", err
	}

	body := fmt.Sprintf("Ваш код для входа в бота «Тайный покупатель»: %s\n\nКод действует %d мин. и подходит только для этого аккаунта Telegram.\n"+
		"Никому не сообщайте код. Если вы его не запрашивали, просто проигнорируйте письмо.", code, int(e.ttl.Minutes()))
	if err := e.mail.Send(ctx, deliver, "Код для входа в бота: "+code, body); err != nil {
		_ = e.store.Repos().OTP.Delete(ctx, p.TgID) // код не доставлен: отзываем, пользователь запросит заново
		return "", fmt.Errorf("%w: %v", ErrMailFailed, err)
	}
	return canonical, nil
}

// VerifyCode проверяет код. При успехе создаёт активного пользователя (по почте, без одобрения админа).
func (e *EmailAuth) VerifyCode(ctx context.Context, p Profile, code string) (*domain.User, error) {
	code = strings.TrimSpace(code)
	var user *domain.User
	// Неверный ввод должен сохраниться (счётчик попыток), поэтому ошибки кода возвращаются из
	// отдельной функции вне транзакции, которая откатилась бы вместе с записью попытки.
	var verr error
	err := e.store.WithTx(ctx, func(r storage.Repos) error {
		now := e.now().UTC()
		o, err := r.OTP.Get(ctx, p.TgID)
		if errors.Is(err, storage.ErrNotFound) {
			verr = ErrNoPendingCode
			return nil
		}
		if err != nil {
			return err
		}
		if !now.Before(o.ExpiresAt) {
			verr = ErrCodeExpired
			return r.OTP.Delete(ctx, p.TgID)
		}
		if o.Attempts >= MaxOTPAttempts {
			verr = ErrTooManyAttempts
			return r.OTP.Delete(ctx, p.TgID)
		}
		if !hmac.Equal([]byte(hashCode(o.Salt, code, p.TgID, o.Email)), []byte(o.CodeHash)) {
			n, err := r.OTP.AddAttempt(ctx, p.TgID)
			if err != nil {
				return err
			}
			if n >= MaxOTPAttempts {
				verr = ErrTooManyAttempts
				return r.OTP.Delete(ctx, p.TgID)
			}
			verr = fmt.Errorf("%w (осталось попыток: %d)", ErrCodeInvalid, MaxOTPAttempts-n)
			return nil
		}
		// Код верный: ещё раз проверяем, что аккаунт и почта свободны, и выдаём доступ.
		if _, err := r.Users.Get(ctx, p.TgID); err == nil {
			verr = ErrForbidden
			return r.OTP.Delete(ctx, p.TgID)
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if err := owner(ctx, r, o.Email, p.TgID, now); err != nil {
			if errors.Is(err, ErrEmailTaken) {
				verr = err
				return r.OTP.Delete(ctx, p.TgID)
			}
			return err
		}
		user = &domain.User{
			TgID: p.TgID, Role: domain.RoleBuyer, Status: domain.StatusActive, Lang: "ru",
			FirstName: p.FirstName, Username: p.Username, Email: o.Email, CreatedAt: now, UpdatedAt: now,
		}
		if err := r.Users.Create(ctx, user); err != nil {
			return err
		}
		if err := r.OTP.Delete(ctx, p.TgID); err != nil { // код одноразовый
			return err
		}
		return r.Audit.Add(ctx, &domain.AuditEntry{
			Action: "auth.email", Entity: "user", EntityID: fmt.Sprint(p.TgID), Details: MaskEmail(o.Email), At: now,
		})
	})
	if err != nil {
		return nil, err
	}
	if verr != nil {
		return nil, verr
	}
	return user, nil
}
