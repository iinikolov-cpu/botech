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
	if t.Status == domain.TaskRework && c.Report != nil && c.Report.AdminComment != "" {
		sb.WriteString("\n" + i18n.T(l, "task_rework_head", esc(c.Report.AdminComment)) + "\n\n")
	}
	if c.Comp != nil && c.Comp.Status == domain.CompRejected {
		sb.WriteString("\n" + i18n.T(l, "comp_rejected", esc(c.Comp.AdminComment)) + "\n\n")
	}
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
	seller := body.Kind == domain.KindSeller // у продавца нет промокода, айтема и компенсации
	canHavePromo := !seller && (t.Status == domain.TaskAccepted || t.Status == domain.TaskExpired)
	switch {
	case c.Promo != nil && c.Promo.Outcome == domain.PromoActive:
		sb.WriteString("\n" + i18n.T(l, "promo_line", esc(c.Promo.Code)) + "\n")
	case canHavePromo && c.Promo == nil:
		sb.WriteString("\n" + i18n.T(l, "promo_pending") + "\n")
	}

	needItem := false // покупатель ещё не выбрал товар: отчёт пока недоступен
	if !seller {
		active := t.Status == domain.TaskAccepted || t.Status == domain.TaskExpired
		switch {
		case c.Item != nil:
			sb.WriteString("\n🛒 Товар для покупки: " + itemText(c.Item) + "\n")
		case active:
			sb.WriteString("\n" + i18n.T(l, "item_pending") + "\n")
			needItem = true
		}
	}

	id := itoa(t.ID)
	var rows [][]models.InlineKeyboardButton
	switch t.Status {
	case domain.TaskSent, domain.TaskCreated: // «создано» бывает только в момент доставки
		rows = append(rows, row(btn(i18n.T(l, "btn_accept"), "tsk:ac:"+id), btn(i18n.T(l, "btn_decline"), "tsk:dc:"+id)))
	case domain.TaskAccepted, domain.TaskExpired:
		if !seller && c.Promo == nil {
			rows = append(rows, row(btn(i18n.T(l, "btn_get_promo"), "tsk:pc:"+id)))
		}
		if needItem {
			rows = append(rows, row(btn(i18n.T(l, "btn_choose_item"), "tsk:it:"+id)))
		} else {
			rows = append(rows, row(btn(i18n.T(l, "btn_report"), "tsk:rp:"+id)))
		}
	case domain.TaskRework:
		rows = append(rows, row(btn(i18n.T(l, "btn_fix_report"), "tsk:rp:"+id)))
	}
	if c.Comp != nil && c.Comp.Status == domain.CompRejected {
		rows = append(rows, row(btn(i18n.T(l, "btn_fix_comp"), "tsk:cf:"+id)))
	}
	rows = append(rows, row(btn(i18n.T(l, "btn_back_list"), "tsk:l")))
	return sb.String(), kb(rows...)
}

// itemText название, цена, заметка и ссылка айтема для карточки задания.
func itemText(it *domain.Item) string {
	s := "<b>" + esc(it.Title) + "</b>"
	if it.Price > 0 {
		s += " (" + fmtMoney(it.Price) + " сум)"
	}
	if it.Note != "" {
		s += "\n" + esc(it.Note)
	}
	return s + "\n" + esc(it.URL)
}

// itemsPageSize сколько товаров показывается в списке выбора.
const itemsPageSize = 8

// screenChooseItem список свободных товаров для выбора: tsk:it:<задание>:<стр>.
func (a *App) screenChooseItem(ctx context.Context, l i18n.Lang, taskID int64, page int) (string, *models.InlineKeyboardMarkup, bool) {
	if page < 0 {
		page = 0
	}
	list, total, err := a.items.ListFree(ctx, itemsPageSize, page*itemsPageSize)
	if err != nil {
		a.log.Error("список свободных айтемов", "err", err)
	}
	id := itoa(taskID)
	if total == 0 {
		return i18n.T(l, "items_none"), kb(row(btn(i18n.T(l, "btn_back_list"), "tsk:v:"+id))), false
	}
	var rows [][]models.InlineKeyboardButton
	for _, it := range list {
		label := it.Title
		if it.Price > 0 {
			label += " · " + fmtMoney(it.Price)
		}
		rows = append(rows, row(btn(cut(label, 60), fmt.Sprintf("tsk:ii:%s:%d", id, it.ID))))
	}
	var nav []models.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, btn("‹", fmt.Sprintf("tsk:it:%s:%d", id, page-1)))
	}
	if (page+1)*itemsPageSize < total {
		nav = append(nav, btn("›", fmt.Sprintf("tsk:it:%s:%d", id, page+1)))
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, row(btn(i18n.T(l, "btn_back_list"), "tsk:v:"+id)))
	return i18n.T(l, "items_head", total), kb(rows...), true
}

func (a *App) onTasksCommand(ctx context.Context, b *bot.Bot, upd *models.Update) {
	u := userFrom(ctx)
	if u == nil {
		return
	}
	text, markup := a.screenBuyerTasks(ctx, u)
	a.eat(ctx, b, upd.Message)
	a.sendPanel(ctx, b, u.TgID, text, markup)
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
		label := fmt.Sprintf("#%d %s · %s", c.Task.ID, c.Version.Body.Title, c.Task.Status.Title())
		rows = append(rows, row(btn(cut(label, 60), "tsk:v:"+itoa(c.Task.ID))))
	}
	rows = append(rows, row(btn(i18n.T(l, "btn_kinds"), "kd:s:"+itoa(int64(kindsMask(u.Kinds))))))
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
	case "it": // список свободных товаров: tsk:it:<задание>[:<стр>]
		page := 0
		if len(parts) > 3 {
			page, _ = strconv.Atoi(parts[3])
		}
		text, markup, hasItems := a.screenChooseItem(ctx, l, id, page)
		a.answerCB(ctx, b, cb.ID, "", false)
		if !hasItems {
			a.warnNoItems(ctx, b, card)
		}
		a.edit(ctx, b, cb, text, markup)
		return
	case "ii": // карточка товара: tsk:ii:<задание>:<товар>
		itemID, _ := strconv.ParseInt(lastPart(parts), 10, 64)
		it, err := a.items.Get(ctx, itemID)
		a.answerCB(ctx, b, cb.ID, "", false)
		if err != nil || it.Status != domain.ItemFree {
			text, markup, _ := a.screenChooseItem(ctx, l, id, 0)
			a.edit(ctx, b, cb, text, markup)
			return
		}
		a.edit(ctx, b, cb, "🛒 "+itemText(it), kb(
			row(btn(i18n.T(l, "btn_item_pick"), fmt.Sprintf("tsk:ip:%d:%d", id, it.ID))),
			row(btn(i18n.T(l, "btn_back_items"), "tsk:it:"+itoa(id))),
		))
		return
	case "ip": // выбрать товар: tsk:ip:<задание>:<товар>
		itemID, _ := strconv.ParseInt(lastPart(parts), 10, 64)
		if _, err := a.items.Choose(ctx, u.TgID, id, itemID); err != nil {
			a.answerCB(ctx, b, cb.ID, errText(err), true)
			text, markup, _ := a.screenChooseItem(ctx, l, id, 0)
			a.edit(ctx, b, cb, text, markup)
			return
		}
		a.log.Info("товар выбран", "user", maskID(u.TgID), "task", id, "item", itemID)
		a.answerCB(ctx, b, cb.ID, i18n.T(l, "item_chosen"), false)
		if got, err := a.tasks.Card(ctx, id); err == nil {
			card = got
		}
	case "cf": // исправить данные отклонённой компенсации
		a.startCompFix(ctx, b, u, cb, id)
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
			a.warnNoPromo(ctx, b, card)
			return
		}
		a.answerCB(ctx, b, cb.ID, i18n.T(l, "promo_got"), false)
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
				a.warnNoPromo(ctx, b, card)
			}
			if card.Version.Body.Kind != domain.KindSeller {
				if st, err := a.items.Stats(ctx); err == nil && st.Free == 0 {
					a.warnNoItems(ctx, b, card)
				}
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

// warnNoPromo предупреждает админов, что заданию не хватило промокода, и объясняет причину.
func (a *App) warnNoPromo(ctx context.Context, b *bot.Bot, c *service.TaskCard) {
	var text string
	switch c.PromoReason {
	case service.PromoReasonBusy:
		text = fmt.Sprintf("⚠ Все промокоды заняты активными заданиями, а задание #%d (%s) ждёт код. "+
			"Код освободится, когда кто-то выполнит или отменит своё задание. Можно загрузить новые коды.", c.Task.ID, userLabel(c.User))
	case service.PromoReasonExhausted:
		text = fmt.Sprintf("⚠ Не осталось ни одного промокода со свободными использованиями, а задание #%d (%s) ждёт код. Загрузите новые коды.", c.Task.ID, userLabel(c.User))
	default:
		return
	}
	a.notifyAdminsKeyed(ctx, b, "promo_warn", text, kb(row(btn("🎁 Промокоды", "adm:pr"))))
}
