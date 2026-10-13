package handlers

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"botech/internal/i18n"
	"botech/internal/service"
)

// codeRe код из письма: цифры (допускаются пробелы и дефисы внутри: «123 456»).
var codeRe = regexp.MustCompile(`^\d{3}[ -]?\d{3}$`)

// handleEmailAuth вход незнакомого пользователя по почте сотрудника (AUTH_MODE=email):
// почта, затем код из письма. Существующие пользователи сюда не попадают: их пускает Middleware.
func (a *App) handleEmailAuth(ctx context.Context, b *bot.Bot, upd *models.Update, from *models.User, chatID int64, text string) {
	defer a.lockUser(from.ID)()
	l := i18n.RU
	p := service.Profile{TgID: from.ID, FirstName: from.FirstName, Username: from.Username}
	text = strings.TrimSpace(text)
	domains := strings.Join(a.emailAuth.Domains(), ", ")
	reply := func(msg string) { a.sendKeyed(ctx, b, chatID, "auth", msg, nil) }

	switch {
	case codeRe.MatchString(text) && a.emailAuth.HasPending(ctx, from.ID):
		a.eat(ctx, b, upd.Message) // код не должен оставаться в чате
		u, err := a.emailAuth.VerifyCode(ctx, p, strings.NewReplacer(" ", "", "-", "").Replace(text))
		if err != nil {
			a.log.Info("неудачная проверка кода", "user", maskID(from.ID), "reason", err.Error())
			reply(i18n.T(l, "auth_error", esc(err.Error())))
			return
		}
		a.log.Info("вход по почте", "user", maskID(u.TgID), "email", service.MaskEmail(u.Email))
		a.deleteKeyed(ctx, b, chatID, "auth")
		a.notifyAdmins(ctx, b, "✅ Вошёл по почте: "+userLabel(u)+" ("+esc(u.Email)+")", nil)
		a.send(ctx, b, chatID, i18n.T(l, "auth_ok"), nil)
		a.askKinds(ctx, b, u) // дальше пользователь выбирает, какие задания готов делать
	case strings.Contains(text, "@"):
		a.eat(ctx, b, upd.Message)
		email, err := a.emailAuth.RequestCode(ctx, p, text)
		switch {
		case err == nil:
			a.log.Info("код отправлен", "user", maskID(from.ID), "email", service.MaskEmail(email))
			ttl := int(a.emailAuth.TTL().Minutes())
			reply(i18n.T(l, "auth_code_sent", esc(email), service.OTPLength, ttl))
		case isAuthUserError(err):
			reply(i18n.T(l, "auth_error", esc(err.Error())))
		default:
			a.log.Error("отправка кода на почту", "user", maskID(from.ID), "err", err)
			reply(i18n.T(l, "auth_error", esc(service.ErrMailFailed.Error())))
		}
	default:
		if !a.noAccess.Allow(from.ID) {
			return
		}
		if a.emailAuth.HasPending(ctx, from.ID) {
			reply(i18n.T(l, "auth_code_hint", service.OTPLength))
			return
		}
		reply(i18n.T(l, "auth_ask_email", esc(domains)))
	}
}

// isAuthUserError ошибки, которые можно показать пользователю как есть (не техническая неполадка).
func isAuthUserError(err error) bool {
	var tm *service.TooManyRequestsError
	switch {
	case errors.Is(err, service.ErrBadEmail), errors.Is(err, service.ErrEmailTaken), errors.Is(err, service.ErrForbidden):
		return true
	case errors.As(err, &tm):
		return true
	}
	return false
}
