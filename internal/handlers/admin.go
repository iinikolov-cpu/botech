package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"botech/internal/domain"
	"botech/internal/service"
)

const pageSize = 8

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

// Фильтры списка покупателей: буква в callback-данных -> статус.
var statusByLetter = map[string]domain.UserStatus{
	"p": domain.StatusPending,
	"a": domain.StatusActive,
	"b": domain.StatusBlocked,
}

var statusTitle = map[domain.UserStatus]string{
	domain.StatusPending: "⏳ ожидает",
	domain.StatusActive:  "✅ активен",
	domain.StatusBlocked: "⛔ заблокирован",
}

func letterOf(s domain.UserStatus) string {
	for l, st := range statusByLetter {
		if st == s {
			return l
		}
	}
	return "a"
}

func (a *App) onAdmin(ctx context.Context, b *bot.Bot, upd *models.Update) {
	text, markup := a.screenHome(ctx)
	a.send(ctx, b, userFrom(ctx).TgID, text, markup)
}

// onAdminCallback разбирает "adm:<раздел>:<аргументы...>" и показывает нужный экран.
func (a *App) onAdminCallback(ctx context.Context, b *bot.Bot, upd *models.Update) {
	cb := upd.CallbackQuery
	admin := userFrom(ctx)
	parts := strings.Split(cb.Data, ":")
	if len(parts) < 2 {
		return
	}
	var (
		text   string
		markup *models.InlineKeyboardMarkup
		toast  string
	)
	arg := func(i int) string {
		if i < len(parts) {
			return parts[i]
		}
		return ""
	}
	id64 := func(i int) int64 { v, _ := strconv.ParseInt(arg(i), 10, 64); return v }

	switch parts[1] {
	case "home":
		text, markup = a.screenHome(ctx)
	case "u": // список: adm:u:<p|a|b>:<страница>
		page, _ := strconv.Atoi(arg(3))
		text, markup = a.screenUsers(ctx, statusByLetter[arg(2)], page)
	case "uc": // карточка: adm:uc:<id>
		text, markup = a.screenUser(ctx, id64(2))
	case "us": // смена статуса: adm:us:<id>:<p|a|b>
		text, markup, toast = a.actionSetStatus(ctx, b, admin, id64(2), statusByLetter[arg(3)])
	case "inv":
		text, markup = a.screenInvites(ctx)
	case "invn": // создать инвайт: adm:invn:<число мест>
		n, _ := strconv.Atoi(arg(2))
		text, markup = a.actionCreateInvite(ctx, admin, n)
	case "invr": // отозвать: adm:invr:<код>
		if err := a.access.RevokeInvite(ctx, admin.TgID, arg(2)); err != nil {
			a.log.Error("отзыв инвайта", "err", err)
		}
		text, markup = a.screenInvites(ctx)
		toast = "Инвайт отозван"
	case "adms":
		text, markup = a.screenAdmins(ctx)
	case "admr": // снять админа: adm:admr:<id>
		if err := a.access.RemoveAdmin(ctx, admin.TgID, id64(2)); err != nil {
			toast = errText(err)
		} else {
			toast = "Права сняты"
		}
		text, markup = a.screenAdmins(ctx)
	case "log":
		text, markup = a.screenLog(ctx)
	case "sc", "scv", "sca", "sct":
		text, markup = a.scenarioCallback(ctx, b, admin, parts)
	case "tk", "tc", "trv", "trs":
		text, markup, toast = a.taskAdminCallback(ctx, b, admin, parts)
	case "as":
		text, markup, toast = a.assignCallback(ctx, b, admin, parts)
	case "pr", "pra", "prc", "pri", "pru", "prd", "prda", "prdy", "prdl":
		text, markup, toast = a.promoCallback(ctx, b, admin, parts)
	case "cp", "cc", "cpd", "cpy", "rv":
		text, markup, toast = a.compCallback(ctx, b, admin, parts)
	default:
		a.answerCB(ctx, b, cb.ID, "Неизвестное действие", false)
		return
	}
	a.answerCB(ctx, b, cb.ID, toast, false)
	if text != "" {
		a.edit(ctx, b, cb, text, markup)
	}
}

// errText переводит ошибку сервиса в короткий текст для админа.
func errText(err error) string {
	switch {
	case errors.Is(err, service.ErrForbidden), errors.Is(err, service.ErrNotFound):
		return err.Error()
	default:
		return "Ошибка, подробности в логах"
	}
}

func (a *App) screenHome(ctx context.Context) (string, *models.InlineKeyboardMarkup) {
	_, nPending, _ := a.access.Users(ctx, domain.RoleBuyer, domain.StatusPending, 1, 0)
	_, nActive, _ := a.access.Users(ctx, domain.RoleBuyer, domain.StatusActive, 1, 0)

	buyers := "👥 Покупатели"
	if nPending > 0 {
		buyers += fmt.Sprintf(" (заявок: %d)", nPending)
	}
	text := fmt.Sprintf("<b>Админ-панель</b>\n\nАктивных покупателей: %d\nЗаявок на рассмотрении: %d", nActive, nPending)
	if st, err := a.promos.Stats(ctx); err == nil {
		text += fmt.Sprintf("\nПромокодов свободно: %d", st.Free)
		if st.Free <= a.promoLow {
			text += " ⚠"
		}
	}
	if _, n, sum, err := a.reports.CompPage(ctx, domain.CompPending, 1, 0); err == nil && n > 0 {
		text += fmt.Sprintf("\nК выплате: %d (%s сум)", n, fmtMoney(sum))
	}
	return text, kb(
		row(btn(buyers, "adm:u:"+pendingOrActive(nPending)+":0")),
		row(btn("📌 Задания", "adm:tk:a:0"), btn("➕ Назначить", "adm:as:0")),
		row(btn("📋 Сценарии", "adm:sc"), btn("🎁 Промокоды", "adm:pr")),
		row(btn("💰 Компенсации", "adm:cp:w:0"), btn("🎟 Инвайты", "adm:inv")),
		row(btn("🛡 Админы", "adm:adms"), btn("📜 Журнал", "adm:log")),
	)
}

// pendingOrActive: если есть заявки, сразу открываем их.
func pendingOrActive(nPending int) string {
	if nPending > 0 {
		return "p"
	}
	return "a"
}

func (a *App) screenUsers(ctx context.Context, status domain.UserStatus, page int) (string, *models.InlineKeyboardMarkup) {
	if status == "" {
		status = domain.StatusActive
	}
	if page < 0 {
		page = 0
	}
	list, total, err := a.access.Users(ctx, domain.RoleBuyer, status, pageSize, page*pageSize)
	if err != nil {
		a.log.Error("список покупателей", "err", err)
		return "Не удалось загрузить список.", kb(row(btn("« Назад", "adm:home")))
	}
	l := letterOf(status)
	var rows [][]models.InlineKeyboardButton
	// Вкладки-фильтры.
	rows = append(rows, row(
		btn(mark("Заявки", status == domain.StatusPending), "adm:u:p:0"),
		btn(mark("Активные", status == domain.StatusActive), "adm:u:a:0"),
		btn(mark("Блок", status == domain.StatusBlocked), "adm:u:b:0"),
	))
	for _, u := range list {
		name := strings.TrimSpace(u.FirstName)
		if name == "" {
			name = "без имени"
		}
		if u.Username != "" {
			name += " @" + u.Username
		}
		rows = append(rows, row(btn(name, "adm:uc:"+itoa(u.TgID))))
	}
	var nav []models.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, btn("‹", fmt.Sprintf("adm:u:%s:%d", l, page-1)))
	}
	if (page+1)*pageSize < total {
		nav = append(nav, btn("›", fmt.Sprintf("adm:u:%s:%d", l, page+1)))
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, row(btn("« Назад", "adm:home")))
	text := fmt.Sprintf("<b>Покупатели</b>: %s (%d)", strings.TrimPrefix(statusTitle[status], "✅ "), total)
	if total == 0 {
		text += "\n\nСписок пуст."
	}
	return text, kb(rows...)
}

func mark(s string, on bool) string {
	if on {
		return "• " + s
	}
	return s
}

func (a *App) screenUser(ctx context.Context, id int64) (string, *models.InlineKeyboardMarkup) {
	u, err := a.access.Lookup(ctx, id)
	if err != nil || u == nil {
		return "Пользователь не найден.", kb(row(btn("« Назад", "adm:home")))
	}
	text := fmt.Sprintf("%s\nСтатус: %s\nВ системе с: %s",
		userLabel(u), statusTitle[u.Status], a.fmtTime(u.CreatedAt))
	var actions []models.InlineKeyboardButton
	if u.Role == domain.RoleBuyer {
		switch u.Status {
		case domain.StatusPending:
			actions = row(btn("✅ Одобрить", "adm:us:"+itoa(id)+":a"), btn("⛔ Отклонить", "adm:us:"+itoa(id)+":b"))
		case domain.StatusActive:
			actions = row(btn("⛔ Заблокировать", "adm:us:"+itoa(id)+":b"))
		case domain.StatusBlocked:
			actions = row(btn("♻ Разблокировать", "adm:us:"+itoa(id)+":a"))
		}
	} else {
		text += "\nРоль: админ"
	}
	rows := [][]models.InlineKeyboardButton{}
	if actions != nil {
		rows = append(rows, actions)
	}
	rows = append(rows, row(btn("« К списку", "adm:u:"+letterOf(u.Status)+":0")))
	return text, kb(rows...)
}

// actionSetStatus меняет статус и уведомляет покупателя об одобрении.
func (a *App) actionSetStatus(ctx context.Context, b *bot.Bot, admin *domain.User, id int64, st domain.UserStatus) (string, *models.InlineKeyboardMarkup, string) {
	prev, _ := a.access.Lookup(ctx, id)
	u, err := a.access.SetStatus(ctx, admin.TgID, id, st)
	if err != nil {
		text, markup := a.screenUser(ctx, id)
		return text, markup, errText(err)
	}
	a.log.Info("смена статуса", "admin", maskID(admin.TgID), "user", maskID(id), "status", st)
	toast := "Готово"
	if prev != nil && prev.Status == domain.StatusPending && st == domain.StatusActive {
		a.send(ctx, b, id, "Ваша заявка одобрена. Нажмите /start, чтобы начать.", nil)
		toast = "Одобрено"
	}
	text, markup := a.screenUser(ctx, u.TgID)
	return text, markup, toast
}

func (a *App) screenInvites(ctx context.Context) (string, *models.InlineKeyboardMarkup) {
	list, err := a.access.ActiveInvites(ctx)
	if err != nil {
		a.log.Error("список инвайтов", "err", err)
	}
	var sb strings.Builder
	sb.WriteString("<b>Инвайты</b>\nСсылка действует 7 дней. Пользователь входит по ссылке или отправляет боту код.\n")
	rows := [][]models.InlineKeyboardButton{
		row(btn("➕ На 1 чел.", "adm:invn:1"), btn("➕ На 5", "adm:invn:5"), btn("➕ На 20", "adm:invn:20")),
	}
	if len(list) > 0 {
		sb.WriteString("\n<b>Действующие:</b>\n")
	}
	for _, i := range list {
		fmt.Fprintf(&sb, "<code>%s</code> (%d/%d), до %s\n", i.Code, i.UsedCount, i.MaxUses, a.fmtTime(i.ExpiresAt))
		rows = append(rows, row(btn("🗑 Отозвать "+i.Code, "adm:invr:"+i.Code)))
	}
	rows = append(rows, row(btn("« Назад", "adm:home")))
	return sb.String(), kb(rows...)
}

func (a *App) actionCreateInvite(ctx context.Context, admin *domain.User, n int) (string, *models.InlineKeyboardMarkup) {
	inv, err := a.access.CreateInvite(ctx, admin.TgID, n, 7*24*time.Hour)
	if err != nil {
		a.log.Error("создание инвайта", "err", err)
		return "Не удалось создать инвайт.", kb(row(btn("« Назад", "adm:inv")))
	}
	link := "(имя бота неизвестно)"
	if a.botUsername != "" {
		link = fmt.Sprintf("https://t.me/%s?start=%s", a.botUsername, inv.Code)
	}
	text := fmt.Sprintf("<b>Инвайт создан</b> (мест: %d, до %s)\n\nСсылка:\n%s\n\nКод: <code>%s</code>",
		inv.MaxUses, a.fmtTime(inv.ExpiresAt), link, inv.Code)
	return text, kb(row(btn("« К инвайтам", "adm:inv")))
}

func (a *App) screenAdmins(ctx context.Context) (string, *models.InlineKeyboardMarkup) {
	admins, err := a.access.ActiveAdmins(ctx)
	if err != nil {
		a.log.Error("список админов", "err", err)
	}
	var sb strings.Builder
	sb.WriteString("<b>Админы</b>\nДобавить: <code>/addadmin 123456789</code>\nСнять: <code>/rmadmin 123456789</code>\n(Telegram ID человека можно узнать у бота @userinfobot)\n\n")
	rows := [][]models.InlineKeyboardButton{}
	for _, u := range admins {
		fmt.Fprintf(&sb, "• %s (<code>%d</code>)\n", userLabel(u), u.TgID)
		rows = append(rows, row(btn("Снять: "+shortName(u), "adm:admr:"+itoa(u.TgID))))
	}
	rows = append(rows, row(btn("« Назад", "adm:home")))
	return sb.String(), kb(rows...)
}

func shortName(u *domain.User) string {
	if u.FirstName != "" {
		return u.FirstName
	}
	return itoa(u.TgID)
}

func (a *App) screenLog(ctx context.Context) (string, *models.InlineKeyboardMarkup) {
	list, err := a.access.AuditLog(ctx, 20)
	if err != nil {
		a.log.Error("журнал", "err", err)
	}
	var sb strings.Builder
	sb.WriteString("<b>Журнал действий</b> (последние 20)\n\n")
	if len(list) == 0 {
		sb.WriteString("Пока пусто.")
	}
	for _, e := range list {
		fmt.Fprintf(&sb, "%s | админ <code>%d</code> | %s %s %s\n",
			a.fmtTime(e.At), e.AdminID, e.Action, esc(e.EntityID), esc(e.Details))
	}
	return sb.String(), kb(row(btn("« Назад", "adm:home")))
}

// fmtTime показывает время в часовом поясе из настроек; нулевое время = «бессрочно».
func (a *App) fmtTime(t time.Time) string {
	if t.IsZero() {
		return "бессрочно"
	}
	return t.In(a.loc).Format("02.01.2006 15:04")
}

// onAddAdmin: /addadmin <tg_id>
func (a *App) onAddAdmin(ctx context.Context, b *bot.Bot, upd *models.Update) {
	admin := userFrom(ctx)
	id, ok := idArg(upd.Message.Text)
	if !ok {
		a.send(ctx, b, admin.TgID, "Формат: <code>/addadmin 123456789</code>", nil)
		return
	}
	if err := a.access.AddAdmin(ctx, admin.TgID, id); err != nil {
		a.log.Error("добавление админа", "err", err)
		a.send(ctx, b, admin.TgID, esc(errText(err)), nil)
		return
	}
	a.send(ctx, b, admin.TgID, "Готово: пользователь добавлен в админы. Пусть откроет бота и нажмёт /start.", nil)
}

// onRemoveAdmin: /rmadmin <tg_id>
func (a *App) onRemoveAdmin(ctx context.Context, b *bot.Bot, upd *models.Update) {
	admin := userFrom(ctx)
	id, ok := idArg(upd.Message.Text)
	if !ok {
		a.send(ctx, b, admin.TgID, "Формат: <code>/rmadmin 123456789</code>", nil)
		return
	}
	if err := a.access.RemoveAdmin(ctx, admin.TgID, id); err != nil {
		a.send(ctx, b, admin.TgID, esc(errText(err)), nil)
		return
	}
	a.send(ctx, b, admin.TgID, "Готово: права админа сняты.", nil)
}

func idArg(text string) (int64, bool) {
	f := strings.Fields(text)
	if len(f) < 2 {
		return 0, false
	}
	id, err := strconv.ParseInt(f[1], 10, 64)
	return id, err == nil && id > 0
}
