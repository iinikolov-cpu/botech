// Пакет handlers связывает Telegram и сервисы: маршрутизация, middleware, экраны.
package handlers

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"botech/internal/domain"
	"botech/internal/i18n"
	"botech/internal/service"
)

// Services набор сервисов бизнес-логики, нужных обработчикам.
type Services struct {
	Access    *service.Access
	Scenarios *service.Scenarios
	Tasks     *service.Tasks
	Dialog    *service.Dialog
	Promos    *service.Promos
	Reports   *service.Reports
	Reminders *service.Reminders
	Settings  *service.Settings
	Backups   *service.Backups

	BackupChatID int64 // кому отправлять ежедневную копию базы (первый админ)
}

// App хранит зависимости обработчиков.
type App struct {
	access      *service.Access
	scenarios   *service.Scenarios
	tasks       *service.Tasks
	dialog      *service.Dialog
	promos      *service.Promos
	reports     *service.Reports
	reminders   *service.Reminders
	settings    *service.Settings
	backups     *service.Backups
	backupChat  int64
	log         *slog.Logger
	loc         *time.Location
	botUsername string

	panels *panelStore // «живое» меню каждого пользователя (см. panel.go)

	badInvites *limiter // неудачные попытки ввода кода незнакомцами
	noAccess   *limiter // ответы «нет доступа» (чтобы не спамить в ответ на спам)
}

// New создаёт приложение обработчиков.
func New(svc Services, log *slog.Logger, loc *time.Location) *App {
	return &App{
		access: svc.Access, scenarios: svc.Scenarios, tasks: svc.Tasks, dialog: svc.Dialog,
		promos: svc.Promos, reports: svc.Reports, reminders: svc.Reminders, settings: svc.Settings, backups: svc.Backups, backupChat: svc.BackupChatID, log: log, loc: loc,
		panels:     newPanelStore(),
		badInvites: newLimiter(5, time.Hour),
		noAccess:   newLimiter(1, 30*time.Second),
	}
}

// Register регистрирует маршруты. Имена команд задаются БЕЗ слеша (так требует библиотека). Middleware доступа подключается отдельно (Middleware()).
func (a *App) Register(b *bot.Bot, username string) {
	a.botUsername = username
	b.RegisterHandler(bot.HandlerTypeMessageText, "start", bot.MatchTypeCommandStartOnly, a.onStart)
	b.RegisterHandler(bot.HandlerTypeMessageText, "help", bot.MatchTypeCommand, a.onHelp)
	b.RegisterHandler(bot.HandlerTypeMessageText, "admin", bot.MatchTypeCommand, a.adminOnly(a.onAdmin))
	b.RegisterHandler(bot.HandlerTypeMessageText, "addadmin", bot.MatchTypeCommand, a.adminOnly(a.onAddAdmin))
	b.RegisterHandler(bot.HandlerTypeMessageText, "rmadmin", bot.MatchTypeCommand, a.adminOnly(a.onRemoveAdmin))
	b.RegisterHandler(bot.HandlerTypeMessageText, "tasks", bot.MatchTypeCommand, a.onTasksCommand)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "adm:", bot.MatchTypePrefix, a.adminOnly(a.onAdminCallback))
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "tsk:", bot.MatchTypePrefix, a.onTaskCallback)
	b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "rpt:", bot.MatchTypePrefix, a.onReportCallback)
	// Файлы сценариев (документы) принимаем только от админов.
	b.RegisterHandlerMatchFunc(func(u *models.Update) bool {
		return u.Message != nil && u.Message.Document != nil
	}, a.adminOnly(a.onDocument))
}

// DefaultHandler обрабатывает всё, что не подошло под маршруты: ответы в пошаговых диалогах
// (отчёт, загрузка промокодов) и свободный текст.
func (a *App) DefaultHandler(ctx context.Context, b *bot.Bot, upd *models.Update) {
	u := userFrom(ctx)
	if u == nil || upd.Message == nil {
		return
	}
	m := upd.Message
	switch state, _ := a.dialog.Get(ctx, u.TgID, nil); state {
	case reportDialog:
		if st, ok := a.loadReport(ctx, u.TgID); ok {
			a.onReportMessage(ctx, b, u, st, m)
			return
		}
	case promoAddDialog:
		if u.IsAdmin() && m.Text != "" {
			a.eat(ctx, b, m)
			a.importPromos(ctx, b, u, m.Text)
			return
		}
	case settingDialog:
		if u.IsAdmin() && m.Text != "" {
			a.eat(ctx, b, m)
			a.onSettingValue(ctx, b, u, m.Text)
			return
		}
	case reworkDialog:
		if u.IsAdmin() && m.Text != "" {
			a.eat(ctx, b, m)
			a.onReworkComment(ctx, b, u, m.Text)
			return
		}
	case compRejectDialog:
		if u.IsAdmin() && m.Text != "" {
			a.eat(ctx, b, m)
			a.onCompRejectComment(ctx, b, u, m.Text)
			return
		}
	case promoDelDialog:
		if u.IsAdmin() && m.Text != "" {
			a.eat(ctx, b, m)
			a.deletePromos(ctx, b, u, m.Text)
			return
		}
	}
	if m.Text != "" {
		a.send(ctx, b, u.TgID, i18n.T(lang(u), "help_buyer"), nil)
	}
}

func lang(u *domain.User) i18n.Lang {
	if u == nil || u.Lang == "" {
		return i18n.RU
	}
	return i18n.Lang(u.Lang)
}

// Middleware проверка доступа: для каждого обновления определяет пользователя.
// Незнакомцы и заблокированные видят только «Нет доступа».
func (a *App) Middleware(next bot.HandlerFunc) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, upd *models.Update) {
		var (
			from   *models.User
			chatID int64
			text   string
		)
		switch {
		case upd.Message != nil && upd.Message.From != nil:
			from, chatID, text = upd.Message.From, upd.Message.Chat.ID, upd.Message.Text
			if upd.Message.Chat.Type != models.ChatTypePrivate {
				return // работаем только в личных сообщениях
			}
		case upd.CallbackQuery != nil:
			from, chatID = &upd.CallbackQuery.From, upd.CallbackQuery.From.ID
		default:
			return
		}
		if from.IsBot {
			return
		}

		u, err := a.access.Lookup(ctx, from.ID)
		if err != nil {
			a.log.Error("ошибка чтения пользователя", "user", maskID(from.ID), "err", err)
			return
		}
		if u == nil {
			a.handleStranger(ctx, b, upd, from, chatID, text)
			return
		}
		switch u.Status {
		case domain.StatusBlocked:
			a.deny(ctx, b, upd, from.ID, i18n.T(lang(u), "no_access"))
			return
		case domain.StatusPending:
			a.deny(ctx, b, upd, from.ID, i18n.T(lang(u), "pending"))
			return
		}
		a.access.TouchProfile(ctx, u, service.Profile{TgID: from.ID, FirstName: from.FirstName, Username: from.Username})
		next(withUser(ctx, u), b, upd)
	}
}

// deny отвечает отказом (для кнопок всплывающим окном), с ограничением частоты.
func (a *App) deny(ctx context.Context, b *bot.Bot, upd *models.Update, id int64, text string) {
	if upd.CallbackQuery != nil {
		a.answerCB(ctx, b, upd.CallbackQuery.ID, text, true)
		return
	}
	if a.noAccess.Allow(id) {
		a.send(ctx, b, id, esc(text), nil)
	}
}

// codeFromText достаёт код из "/start КОД" или из обычного сообщения-кода.
func codeFromText(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "/start") {
		parts := strings.Fields(text)
		if len(parts) < 2 {
			return ""
		}
		return parts[1]
	}
	if service.LooksLikeCode(text) {
		return text
	}
	return ""
}

// handleStranger: незнакомый пользователь. Пускаем дальше только по валидному инвайту.
func (a *App) handleStranger(ctx context.Context, b *bot.Bot, upd *models.Update, from *models.User, chatID int64, text string) {
	if upd.CallbackQuery != nil {
		a.answerCB(ctx, b, upd.CallbackQuery.ID, i18n.T(i18n.RU, "no_access"), true)
		return
	}
	code := codeFromText(text)
	if code == "" || a.badInvites.Blocked(from.ID) {
		a.deny(ctx, b, upd, from.ID, i18n.T(i18n.RU, "no_access"))
		return
	}
	p := service.Profile{TgID: from.ID, FirstName: from.FirstName, Username: from.Username}
	u, err := a.access.Join(ctx, p, code)
	if err != nil {
		if errors.Is(err, service.ErrInvalidInvite) {
			a.badInvites.Allow(from.ID) // считаем неудачную попытку
			a.log.Info("неверный инвайт", "user", maskID(from.ID))
			a.send(ctx, b, chatID, i18n.T(i18n.RU, "invite_bad"), nil)
		} else {
			a.log.Error("ошибка регистрации по инвайту", "err", err)
		}
		return
	}
	a.log.Info("новая заявка", "user", maskID(u.TgID))
	a.send(ctx, b, chatID, i18n.T(i18n.RU, "join_ok"), nil)
	a.notifyAdminsNewUser(ctx, b, u)
}

// notifyAdminsNewUser рассылает админам заявку с кнопками.
func (a *App) notifyAdminsNewUser(ctx context.Context, b *bot.Bot, u *domain.User) {
	admins, err := a.access.ActiveAdmins(ctx)
	if err != nil {
		a.log.Error("не удалось получить админов", "err", err)
		return
	}
	text := "🆕 Новая заявка: " + userLabel(u)
	markup := kb(row(
		btn("✅ Одобрить", "adm:us:"+itoa(u.TgID)+":a"),
		btn("⛔ Отклонить", "adm:us:"+itoa(u.TgID)+":b"),
	))
	for _, ad := range admins {
		a.send(ctx, b, ad.TgID, text, markup)
	}
}

func (a *App) onStart(ctx context.Context, b *bot.Bot, upd *models.Update) {
	u := userFrom(ctx)
	if u == nil || upd.Message == nil {
		return
	}
	text := i18n.T(lang(u), "welcome_buyer", esc(u.FirstName))
	if u.IsAdmin() {
		text += i18n.T(lang(u), "help_admin")
	}
	a.eat(ctx, b, upd.Message)
	a.sendPanel(ctx, b, u.TgID, text, kb(row(btn(i18n.T(lang(u), "btn_tasks"), "tsk:l"))))
}

func (a *App) onHelp(ctx context.Context, b *bot.Bot, upd *models.Update) {
	u := userFrom(ctx)
	if u == nil {
		return
	}
	text := i18n.T(lang(u), "help_buyer")
	if u.IsAdmin() {
		text += i18n.T(lang(u), "help_admin")
	}
	a.eat(ctx, b, upd.Message)
	a.sendPanel(ctx, b, u.TgID, text, nil)
}

// adminOnly пропускает только активных админов.
func (a *App) adminOnly(h bot.HandlerFunc) bot.HandlerFunc {
	return func(ctx context.Context, b *bot.Bot, upd *models.Update) {
		u := userFrom(ctx)
		if !u.IsAdmin() {
			if upd.CallbackQuery != nil {
				a.answerCB(ctx, b, upd.CallbackQuery.ID, i18n.T(lang(u), "no_access"), true)
			}
			return
		}
		h(ctx, b, upd)
	}
}
