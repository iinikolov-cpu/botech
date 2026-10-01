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
)

// Диалоги, в которых админ пишет свободный комментарий.
const (
	reworkDialog     = "rework"      // комментарий при возврате отчёта на доработку
	compRejectDialog = "comp_reject" // комментарий при отклонении компенсации
)

// idState данные диалога: к какому заданию или компенсации относится комментарий.
type idState struct {
	ID int64 `json:"id"`
}

var compStatusTitle = map[domain.CompStatus]string{
	domain.CompPending:  "к выплате",
	domain.CompPaid:     "выплачено",
	domain.CompRejected: "отклонено, ждём исправления",
}

// compCallback компенсации и отчёты:
//
//	adm:cp:<w|r|d>:<стр>  список (к выплате, отклонено, выплачено)
//	adm:cc:<id>           открыть компенсацию: фото чека с кнопками одним сообщением
//	adm:cv:<id>           перерисовать карточку компенсации на месте
//	adm:cpd/cpy:<id>      выплата (подтверждение и сама отметка)
//	adm:crj/crx:<id>      отклонение компенсации (запрос комментария / отмена)
//	adm:rv:<taskID>       открыть отчёт: сначала медиа, последним сообщением текст с кнопками
//	adm:rr:<taskID>[:ok]  перерисовать текст отчёта на месте (и отметить проверенным)
//	adm:rw/rwx:<taskID>   возврат на доработку (запрос комментария / отмена)
func (a *App) compCallback(ctx context.Context, b *bot.Bot, admin *domain.User, parts []string) (string, *models.InlineKeyboardMarkup, string) {
	arg := func(i int) string {
		if i < len(parts) {
			return parts[i]
		}
		return ""
	}
	id, _ := strconv.ParseInt(arg(2), 10, 64)

	switch parts[1] {
	case "cc":
		a.openComp(ctx, b, admin, id)
		return "", nil, ""
	case "cv":
		t, m := a.compCard(ctx, id)
		return t, m, ""
	case "cpd": // подтверждение выплаты: действие необратимо
		cc, err := a.reports.CompCardByID(ctx, id)
		if err != nil || cc.Comp.Status != domain.CompPending {
			t, m := a.compCard(ctx, id)
			return t, m, "Уже обработано или не найдено"
		}
		text := fmt.Sprintf("Подтвердите: компенсация <b>%s сум</b> для %s выплачена?\nОтметку нельзя отменить.", fmtMoney(cc.Comp.Amount), userLabel(cc.User))
		return text, kb(row(btn("✅ Да, выплачено", "adm:cpy:"+itoa(id)), btn("Назад", "adm:cv:"+itoa(id)))), ""
	case "cpy":
		toast := "Отмечено как выплаченное"
		changed, err := a.reports.MarkPaid(ctx, admin.TgID, id)
		if err != nil {
			a.log.Error("отметка выплаты", "err", err)
			toast = "Ошибка, подробности в логах"
		} else if !changed {
			toast = "Уже обработано"
		} else if cc, err := a.reports.CompCardByID(ctx, id); err == nil {
			a.send(ctx, b, cc.User.TgID, i18n.T(lang(cc.User), "comp_paid_notice", fmtMoney(cc.Comp.Amount), esc(cc.Title)), nil)
			a.log.Info("компенсация выплачена", "admin", maskID(admin.TgID), "compensation", id)
		}
		t, m := a.compCard(ctx, id)
		return t, m, toast
	case "crj": // запросить комментарий для отклонения
		cc, err := a.reports.CompCardByID(ctx, id)
		if err != nil || cc.Comp.Status != domain.CompPending {
			t, m := a.compCard(ctx, id)
			return t, m, "Отклонить можно только компенсацию «к выплате»"
		}
		if err := a.dialog.Set(ctx, admin.TgID, compRejectDialog, idState{ID: id}); err != nil {
			a.log.Error("состояние отклонения компенсации", "err", err)
		}
		return "✖ <b>Отклонение компенсации</b>\nНапишите комментарий для покупателя: что нужно исправить (сумма, чек и т.д.). Он получит его вместе с просьбой прислать данные заново.",
			kb(row(btn("Отмена", "adm:crx:"+itoa(id)))), ""
	case "crx":
		_ = a.dialog.Clear(ctx, admin.TgID)
		t, m := a.compCard(ctx, id)
		return t, m, "Отменено"
	case "rv":
		a.openReport(ctx, b, admin, id)
		return "", nil, ""
	case "rr":
		toast := ""
		if arg(3) == "ok" { // принять отчёт прямо из его экрана
			if changed, err := a.tasks.Review(ctx, admin.TgID, id); err != nil {
				toast = "Ошибка, подробности в логах"
			} else if changed {
				toast = "Отчёт принят"
			} else {
				toast = "Принять можно только отчёт, ожидающий проверки"
			}
		}
		t, m := a.reportText(ctx, id)
		return t, m, toast
	case "rw": // запросить комментарий для возврата на доработку
		card, err := a.tasks.Card(ctx, id)
		if err != nil || card.Task.Status != domain.TaskReported {
			t, m := a.reportText(ctx, id)
			return t, m, "Вернуть на доработку можно только отчёт, ожидающий проверки"
		}
		if err := a.dialog.Set(ctx, admin.TgID, reworkDialog, idState{ID: id}); err != nil {
			a.log.Error("состояние возврата на доработку", "err", err)
		}
		return "↩ <b>Возврат отчёта на доработку</b>\nНапишите комментарий для покупателя: что нужно исправить. Он получит его и сможет заполнить отчёт заново.",
			kb(row(btn("Отмена", "adm:rwx:"+itoa(id)))), ""
	case "rwx":
		_ = a.dialog.Clear(ctx, admin.TgID)
		t, m := a.reportText(ctx, id)
		return t, m, "Отменено"
	}
	// список компенсаций
	status := domain.CompPending
	switch arg(2) {
	case "d":
		status = domain.CompPaid
	case "r":
		status = domain.CompRejected
	}
	page, _ := strconv.Atoi(arg(3))
	t, m := a.screenComps(ctx, status, page)
	return t, m, ""
}

func (a *App) screenComps(ctx context.Context, status domain.CompStatus, page int) (string, *models.InlineKeyboardMarkup) {
	if page < 0 {
		page = 0
	}
	list, total, sum, err := a.reports.CompPage(ctx, status, pageSize, page*pageSize)
	if err != nil {
		a.log.Error("список компенсаций", "err", err)
		return "Не удалось загрузить список.", kb(row(btn("« Назад", "adm:home")))
	}
	f := map[domain.CompStatus]string{domain.CompPending: "w", domain.CompRejected: "r", domain.CompPaid: "d"}[status]
	rows := [][]models.InlineKeyboardButton{row(
		btn(mark("К выплате", status == domain.CompPending), "adm:cp:w:0"),
		btn(mark("Отклонено", status == domain.CompRejected), "adm:cp:r:0"),
		btn(mark("Выплачено", status == domain.CompPaid), "adm:cp:d:0"),
	)}
	for _, c := range list {
		name := strings.TrimSpace(c.User.FirstName)
		if name == "" {
			name = itoa(c.User.TgID)
		}
		warn := ""
		if c.DupReceipt != 0 {
			warn = "⚠ "
		}
		rows = append(rows, row(btn(cut(fmt.Sprintf("%s#%d %s · %s сум", warn, c.Comp.TaskID, name, fmtMoney(c.Comp.Amount)), 60), "adm:cc:"+itoa(c.Comp.ID))))
	}
	var nav []models.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, btn("‹", fmt.Sprintf("adm:cp:%s:%d", f, page-1)))
	}
	if (page+1)*pageSize < total {
		nav = append(nav, btn("›", fmt.Sprintf("adm:cp:%s:%d", f, page+1)))
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, row(btn("« Назад", "adm:home")))
	text := fmt.Sprintf("<b>Компенсации: %s</b>\nЗаписей: %d, на сумму <b>%s сум</b>", compStatusTitle[status], total, fmtMoney(sum))
	if total == 0 {
		text += "\n\nПока пусто."
	}
	return text, kb(rows...)
}

// compCard текст (подпись) и кнопки карточки компенсации.
func (a *App) compCard(ctx context.Context, id int64) (string, *models.InlineKeyboardMarkup) {
	cc, err := a.reports.CompCardByID(ctx, id)
	if err != nil {
		return "Запись не найдена.", kb(row(btn("« К компенсациям", "adm:cp:w:0")))
	}
	c := cc.Comp
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>Компенсация, задание #%d</b>\n«%s»\nПокупатель: %s\nСумма: <b>%s сум</b>\n", c.TaskID, esc(cut(cc.Title, 60)), userLabel(cc.User), fmtMoney(c.Amount))
	switch c.Status {
	case domain.CompPaid:
		fmt.Fprintf(&sb, "Статус: ✅ выплачено %s\n", a.fmtTime(c.PaidAt))
	case domain.CompRejected:
		fmt.Fprintf(&sb, "Статус: ↩ отклонено, ждём исправления\nКомментарий: <i>%s</i>\n", esc(cut(c.AdminComment, 300)))
	default:
		sb.WriteString("Статус: ⏳ к выплате\n")
	}
	if cc.DupReceipt != 0 {
		fmt.Fprintf(&sb, "⚠ Этот файл чека уже прикладывали в задании #%d. Проверьте.\n", cc.DupReceipt)
	}
	var rows [][]models.InlineKeyboardButton
	if c.Status == domain.CompPending {
		rows = append(rows, row(btn("💵 Выплачено", "adm:cpd:"+itoa(id)), btn("✖ Отклонить", "adm:crj:"+itoa(id))))
	}
	rows = append(rows, row(btn("📄 Отчёт", "adm:rv:"+itoa(c.TaskID)), btn("« К списку", "adm:cp:"+map[domain.CompStatus]string{domain.CompPending: "w", domain.CompRejected: "r", domain.CompPaid: "d"}[c.Status]+":0")))
	return sb.String(), kb(rows...)
}

// openComp показывает компенсацию одним сообщением: фото чека, а данные и кнопки прямо под ним.
// Так кнопки всегда рядом с чеком и не уезжают за длинной лентой сообщений.
func (a *App) openComp(ctx context.Context, b *bot.Bot, admin *domain.User, id int64) {
	cc, err := a.reports.CompCardByID(ctx, id)
	if err != nil {
		a.sendPanel(ctx, b, admin.TgID, "Запись не найдена.", kb(row(btn("« К компенсациям", "adm:cp:w:0"))))
		return
	}
	text, markup := a.compCard(ctx, id)
	msg, err := b.SendPhoto(ctx, &bot.SendPhotoParams{
		ChatID: admin.TgID, Photo: &models.InputFileString{Data: cc.Comp.ReceiptFileID},
		Caption: cut(text, 1000), ParseMode: models.ParseModeHTML, ReplyMarkup: markup,
	})
	if err == nil && msg != nil {
		a.deleteMsgs(ctx, b, admin.TgID, a.panels.swap(admin.TgID, msg.ID, nil))
	}
	if err != nil { // чек не открылся: показываем данные текстом, чтобы действия остались доступны
		a.log.Warn("не удалось отправить фото чека", "err", err)
		a.sendPanel(ctx, b, admin.TgID, text+"\n⚠ Фото чека не удалось загрузить.", markup)
	}
}

// openReport показывает отчёт: сначала альбомом фото и видео, последним сообщением ответы с кнопками.
func (a *App) openReport(ctx context.Context, b *bot.Bot, admin *domain.User, taskID int64) {
	card, err := a.tasks.Card(ctx, taskID)
	if err != nil || card.Report == nil {
		a.sendPanel(ctx, b, admin.TgID, "Отчёта по этому заданию пока нет.", kb(row(btn("« К заданию", "adm:tc:"+itoa(taskID)))))
		return
	}
	qText := map[string]string{}
	for i, q := range card.Version.Body.Questions {
		qText[q.Key] = fmt.Sprintf("%d. %s", i+1, cut(q.Text, 200))
	}
	var media []mediaItem
	for _, an := range card.Report.Answers {
		if !an.Skipped && (an.Type == domain.QPhoto || an.Type == domain.QVideo) && an.FileID != "" {
			media = append(media, mediaItem{Video: an.Type == domain.QVideo, FileID: an.FileID, Caption: qText[an.Key]})
		}
	}
	album := a.sendMedia(ctx, b, admin.TgID, media)
	text, markup := a.reportText(ctx, taskID)
	a.sendPanel(ctx, b, admin.TgID, text, markup, album...)
}

// reportText ответы отчёта и кнопки решения (без медиа, поэтому подходит и для перерисовки на месте).
func (a *App) reportText(ctx context.Context, taskID int64) (string, *models.InlineKeyboardMarkup) {
	card, err := a.tasks.Card(ctx, taskID)
	if err != nil {
		return "Задание не найдено.", kb(row(btn("« К заданиям", "adm:tk:a:0")))
	}
	rep := card.Report
	if rep == nil {
		return "Отчёта по этому заданию пока нет.", kb(row(btn("« К заданию", "adm:tc:"+itoa(taskID))))
	}
	byKey := map[string]domain.Answer{}
	for _, an := range rep.Answers {
		byKey[an.Key] = an
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>Отчёт по заданию #%d</b>", taskID)
	if rep.Revision > 1 {
		fmt.Fprintf(&sb, " (версия %d, после доработки)", rep.Revision)
	}
	fmt.Fprintf(&sb, "\n«%s», %s\nПокупатель: %s\nПолучен: %s", esc(card.Version.Body.Title), esc(card.Version.Body.Operator), userLabel(card.User), a.fmtTime(rep.SubmittedAt))
	if rep.Late {
		sb.WriteString(" ⏰ после срока")
	}
	fmt.Fprintf(&sb, "\nСтатус задания: <b>%s</b>\n", card.Task.Status.Title())
	if rep.Decision == domain.ReportRework && rep.AdminComment != "" {
		fmt.Fprintf(&sb, "↩ Возвращён на доработку: <i>%s</i>\n", esc(rep.AdminComment))
	}
	sb.WriteString("\n")
	for i, q := range card.Version.Body.Questions {
		an := byKey[q.Key]
		fmt.Fprintf(&sb, "%d. %s\n   <b>%s</b>\n", i+1, esc(q.Text), esc(a.answerText(i18n.RU, an)))
	}
	if card.Comp != nil {
		fmt.Fprintf(&sb, "\n<b>Компенсация:</b> %s сум (%s)\n", fmtMoney(card.Comp.Amount), compStatusTitle[card.Comp.Status])
	}

	var rows [][]models.InlineKeyboardButton
	if card.Task.Status == domain.TaskReported {
		rows = append(rows,
			row(btn("✅ Проверено", "adm:rr:"+itoa(taskID)+":ok"), btn("↩ На доработку", "adm:rw:"+itoa(taskID))))
	}
	if card.Comp != nil {
		rows = append(rows, row(btn("💰 Компенсация", "adm:cc:"+itoa(card.Comp.ID))))
	}
	rows = append(rows, row(btn("« К заданию", "adm:tc:"+itoa(taskID))))
	return sb.String(), kb(rows...)
}

// onReworkComment админ прислал комментарий: возвращаем отчёт покупателю на доработку.
func (a *App) onReworkComment(ctx context.Context, b *bot.Bot, admin *domain.User, text string) {
	var st idState
	if name, _ := a.dialog.Get(ctx, admin.TgID, &st); name != reworkDialog {
		return
	}
	card, changed, err := a.reports.ReturnForRework(ctx, admin.TgID, st.ID, text)
	switch {
	case err != nil:
		a.sendKeyed(ctx, b, admin.TgID, "err", "❌ "+esc(errText(err))+"\nНапишите комментарий ещё раз или нажмите «Отмена» выше.", nil)
		return
	case !changed:
		_ = a.dialog.Clear(ctx, admin.TgID)
		a.sendPanel(ctx, b, admin.TgID, "Вернуть на доработку можно только отчёт, ожидающий проверки. Статус уже изменился.", kb(row(btn("« К заданию", "adm:tc:"+itoa(st.ID)))))
		return
	}
	_ = a.dialog.Clear(ctx, admin.TgID)
	a.log.Info("отчёт возвращён на доработку", "admin", maskID(admin.TgID), "task", st.ID)
	a.sendPanel(ctx, b, admin.TgID, fmt.Sprintf("↩ Отчёт по заданию #%d возвращён на доработку. Покупатель получил ваш комментарий.", st.ID),
		kb(row(btn("« К заданию", "adm:tc:"+itoa(st.ID)))))
	a.send(ctx, b, card.User.TgID,
		i18n.T(lang(card.User), "rework_notice", esc(card.Version.Body.Title), esc(card.Report.AdminComment)),
		kb(row(btn(i18n.T(lang(card.User), "btn_fix_report"), "tsk:rp:"+itoa(st.ID)))))
}

// onCompRejectComment админ прислал комментарий: отклоняем компенсацию.
func (a *App) onCompRejectComment(ctx context.Context, b *bot.Bot, admin *domain.User, text string) {
	var st idState
	if name, _ := a.dialog.Get(ctx, admin.TgID, &st); name != compRejectDialog {
		return
	}
	cc, changed, err := a.reports.RejectCompensation(ctx, admin.TgID, st.ID, text)
	switch {
	case err != nil:
		a.sendKeyed(ctx, b, admin.TgID, "err", "❌ "+esc(errText(err))+"\nНапишите комментарий ещё раз или нажмите «Отмена» выше.", nil)
		return
	case !changed:
		_ = a.dialog.Clear(ctx, admin.TgID)
		a.sendPanel(ctx, b, admin.TgID, "Отклонить можно только компенсацию «к выплате». Статус уже изменился.", kb(row(btn("« К компенсациям", "adm:cp:w:0"))))
		return
	}
	_ = a.dialog.Clear(ctx, admin.TgID)
	a.log.Info("компенсация отклонена", "admin", maskID(admin.TgID), "compensation", st.ID)
	a.sendPanel(ctx, b, admin.TgID, fmt.Sprintf("✖ Компенсация по заданию #%d отклонена. Покупатель получил ваш комментарий.", cc.Comp.TaskID),
		kb(row(btn("« К компенсациям", "adm:cp:r:0"))))
	a.send(ctx, b, cc.User.TgID,
		i18n.T(lang(cc.User), "comp_reject_notice", esc(cc.Title), esc(cc.Comp.AdminComment)),
		kb(row(btn(i18n.T(lang(cc.User), "btn_fix_comp"), "tsk:cf:"+itoa(cc.Comp.TaskID)))))
}
