package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"botech/internal/domain"
	"botech/internal/i18n"
	"botech/internal/service"
)

// renderBuyerTask текст и кнопки карточки задания для покупателя.
// Инструкция берётся из той версии сценария, по которой задание выдано.
func (a *App) renderBuyerTask(c *service.TaskCard) (string, *models.InlineKeyboardMarkup) {
	l := lang(c.User)
	body, t := c.Version.Body, c.Task
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>%s</b>\nОператор: %s\n", esc(body.Title), esc(body.Operator))
	if body.City != "" {
		fmt.Fprintf(&sb, "Город: %s\n", esc(body.City))
	}
	if body.PVZ != "" {
		fmt.Fprintf(&sb, "ПВЗ: %s\n", esc(body.PVZ))
	}
	fmt.Fprintf(&sb, "Статус: %s\n", t.Status.Title())
	switch {
	case !t.DueAt.IsZero():
		fmt.Fprintf(&sb, "Выполнить до: <b>%s</b>\n", a.fmtTime(t.DueAt))
	case t.Status == domain.TaskSent || t.Status == domain.TaskCreated:
		fmt.Fprintf(&sb, "Срок: %d дн. после принятия\n", t.DueDays)
	}
	sb.WriteString("\n<b>Инструкция</b>\n")
	for i, s := range body.Steps {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, esc(s))
	}
	canHavePromo := t.Status == domain.TaskAccepted || t.Status == domain.TaskExpired
	switch {
	case c.Promo != nil:
		sb.WriteString("\n" + i18n.T(l, "promo_line", esc(c.Promo.Code)) + "\n")
	case canHavePromo:
		sb.WriteString("\n" + i18n.T(l, "promo_pending") + "\n")
	}

	id := itoa(t.ID)
	var rows [][]models.InlineKeyboardButton
	switch t.Status {
	case domain.TaskSent, domain.TaskCreated: // «создано» бывает только в момент доставки
		rows = append(rows, row(btn(i18n.T(l, "btn_accept"), "tsk:ac:"+id), btn(i18n.T(l, "btn_decline"), "tsk:dc:"+id)))
	case domain.TaskAccepted, domain.TaskExpired:
		if c.Promo == nil {
			rows = append(rows, row(btn(i18n.T(l, "btn_get_promo"), "tsk:pc:"+id)))
		}
		rows = append(rows, row(btn(i18n.T(l, "btn_report"), "tsk:rp:"+id)))
	}
	rows = append(rows, row(btn(i18n.T(l, "btn_back_list"), "tsk:l")))
	return sb.String(), kb(rows...)
}

func (a *App) onTasksCommand(ctx context.Context, b *bot.Bot, upd *models.Update) {
	u := userFrom(ctx)
	if u == nil {
		return
	}
	text, markup := a.screenBuyerTasks(ctx, u)
	a.send(ctx, b, u.TgID, text, markup)
}

func (a *App) screenBuyerTasks(ctx context.Context, u *domain.User) (string, *models.InlineKeyboardMarkup) {
	l := lang(u)
	list, err := a.tasks.ForUser(ctx, u.TgID)
	if err != nil {
		a.log.Error("список заданий покупателя", "err", err)
		return i18n.T(l, "tasks_empty"), nil
	}
	if len(list) == 0 {
		return i18n.T(l, "tasks_empty"), nil
	}
	var rows [][]models.InlineKeyboardButton
	for _, c := range list {
		label := fmt.Sprintf("%s · %s", c.Version.Body.Title, c.Task.Status.Title())
		rows = append(rows, row(btn(cut(label, 60), "tsk:v:"+itoa(c.Task.ID))))
	}
	return i18n.T(l, "tasks_title"), kb(rows...)
}

// onTaskCallback кнопки покупателя: tsk:l, tsk:v:<id>, tsk:ac:<id>, tsk:dc:<id>, tsk:dy:<id>, tsk:rp:<id>.
func (a *App) onTaskCallback(ctx context.Context, b *bot.Bot, upd *models.Update) {
	cb := upd.CallbackQuery
	u := userFrom(ctx)
	if u == nil {
		return
	}
	l := lang(u)
	parts := strings.Split(cb.Data, ":")
	if len(parts) < 2 {
		return
	}
	if parts[1] == "l" {
		a.answerCB(ctx, b, cb.ID, "", false)
		text, markup := a.screenBuyerTasks(ctx, u)
		a.edit(ctx, b, cb, text, markup)
		return
	}
	if len(parts) < 3 {
		return
	}
	id, _ := strconv.ParseInt(parts[2], 10, 64)

	// Чужие и несуществующие задания выглядят одинаково: «не найдено».
	card, err := a.tasks.Card(ctx, id)
	if err != nil || card.Task.UserID != u.TgID {
		a.answerCB(ctx, b, cb.ID, i18n.T(l, "task_not_found"), true)
		return
	}

	switch parts[1] {
	case "v":
		a.answerCB(ctx, b, cb.ID, "", false)
	case "rp":
		a.startReport(ctx, b, u, cb, id)
		return
	case "pc": // получить промокод, если при принятии пул был пуст
		got, err := a.tasks.IssuePromo(ctx, u.TgID, id)
		if err != nil {
			a.log.Error("выдача промокода по кнопке", "err", err)
			a.answerCB(ctx, b, cb.ID, i18n.T(l, "task_not_found"), true)
			return
		}
		card = got
		if card.Promo == nil {
			a.answerCB(ctx, b, cb.ID, i18n.T(l, "promo_none_yet"), true)
			a.notifyAdmins(ctx, b, fmt.Sprintf("⚠ Пул промокодов пуст: %s ждёт код (задание #%d). Загрузите коды в разделе «Промокоды».", userLabel(u), id), kb(row(btn("🎁 Промокоды", "adm:pr"))))
			return
		}
		a.answerCB(ctx, b, cb.ID, i18n.T(l, "promo_got"), false)
		a.warnPool(ctx, b, card)
	case "dc": // сначала подтверждение: отказ необратим
		a.answerCB(ctx, b, cb.ID, "", false)
		a.edit(ctx, b, cb, i18n.T(l, "confirm_decline"), kb(row(
			btn(i18n.T(l, "btn_yes_decline"), "tsk:dy:"+itoa(id)),
			btn(i18n.T(l, "btn_no"), "tsk:v:"+itoa(id)),
		)))
		return
	case "ac", "dy":
		var changed bool
		if parts[1] == "ac" {
			card, changed, err = a.tasks.Accept(ctx, u.TgID, id)
		} else {
			card, changed, err = a.tasks.Decline(ctx, u.TgID, id)
		}
		if err != nil {
			a.log.Error("смена статуса задания", "err", err)
			a.answerCB(ctx, b, cb.ID, "Ошибка, попробуйте ещё раз", true)
			return
		}
		switch {
		case !changed:
			a.answerCB(ctx, b, cb.ID, i18n.T(l, "task_already"), false)
		case card.Task.Status == domain.TaskAccepted:
			a.answerCB(ctx, b, cb.ID, i18n.T(l, "task_accepted", a.fmtTime(card.Task.DueAt)), true)
			a.notifyCreator(ctx, b, card, "✅ принял задание", "срок до "+a.fmtTime(card.Task.DueAt))
			if card.Promo == nil {
				a.notifyAdmins(ctx, b, fmt.Sprintf("⚠ Пул промокодов пуст: %s принял задание #%d, но код не выдан. Покупатель сможет получить его кнопкой позже. Загрузите коды.", userLabel(card.User), card.Task.ID), kb(row(btn("🎁 Промокоды", "adm:pr"))))
			} else {
				a.warnPool(ctx, b, card)
			}
		default:
			a.answerCB(ctx, b, cb.ID, i18n.T(l, "task_declined"), true)
			a.notifyCreator(ctx, b, card, "❌ отказался от задания", "")
		}
		a.log.Info("действие по заданию", "user", maskID(u.TgID), "task", id, "status", card.Task.Status, "changed", changed)
	}
	text, markup := a.renderBuyerTask(card)
	a.edit(ctx, b, cb, text, markup)
}

// notifyCreator сообщает админу, назначившему задание, о решении покупателя.
func (a *App) notifyCreator(ctx context.Context, b *bot.Bot, c *service.TaskCard, what, extra string) {
	text := fmt.Sprintf("%s %s: «%s» (задание #%d)", userLabel(c.User), what, esc(c.Version.Body.Title), c.Task.ID)
	if extra != "" {
		text += ", " + esc(extra)
	}
	a.send(ctx, b, c.Task.CreatedBy, text, kb(row(btn("Открыть задание", "adm:tc:"+itoa(c.Task.ID)))))
}

// deliverTask отправляет покупателю сообщение с заданием и фиксирует результат доставки в истории.
func (a *App) deliverTask(ctx context.Context, b *bot.Bot, taskID int64) bool {
	card, err := a.tasks.Card(ctx, taskID)
	if err != nil {
		a.log.Error("чтение задания для отправки", "task", taskID, "err", err)
		return false
	}
	text, markup := a.renderBuyerTask(card)
	text = i18n.T(lang(card.User), "task_new_head") + "\n\n" + text
	if err := a.trySend(ctx, b, card.User.TgID, text, markup); err != nil {
		a.log.Warn("задание не доставлено", "task", taskID, "user", maskID(card.User.TgID), "err", err)
		_ = a.tasks.MarkSendFailed(ctx, taskID, "не доставлено (покупатель заблокировал бота или недоступен)")
		return false
	}
	if err := a.tasks.MarkSent(ctx, taskID); err != nil {
		a.log.Error("фиксация отправки", "task", taskID, "err", err)
	}
	return true
}

// warnPool предупреждает админов, когда после выдачи в пуле осталось ровно пороговое число кодов
// (или ноль). Равенство, а не «меньше», чтобы предупреждение приходило один раз, а не при каждой выдаче.
func (a *App) warnPool(ctx context.Context, b *bot.Bot, c *service.TaskCard) {
	if !c.NewPromo || (c.PoolLeft != a.promoLow && c.PoolLeft != 0) {
		return
	}
	text := fmt.Sprintf("⚠ Промокодов в пуле осталось: <b>%d</b>. Загрузите новые.", c.PoolLeft)
	a.notifyAdmins(ctx, b, text, kb(row(btn("➕ Загрузить коды", "adm:pra"))))
}
