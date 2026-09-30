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

// compCallback экраны компенсаций и просмотра отчёта:
// adm:cp:<w|d>:<стр>, adm:cc:<id>, adm:cpd:<id>, adm:cpy:<id>, adm:rv:<taskID>.
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
		t, m := a.screenComp(ctx, b, admin, id)
		return t, m, ""
	case "cpd": // подтверждение выплаты: действие необратимо
		cc, err := a.reports.CompCardByID(ctx, id)
		if err != nil || cc.Comp.Status != domain.CompPending {
			t, m := a.screenComp(ctx, b, admin, id)
			return t, m, "Уже выплачено или не найдено"
		}
		text := fmt.Sprintf("Подтвердите: компенсация <b>%s сум</b> для %s выплачена?\nОтметку нельзя отменить.", fmtMoney(cc.Comp.Amount), userLabel(cc.User))
		return text, kb(row(btn("✅ Да, выплачено", "adm:cpy:"+itoa(id)), btn("Назад", "adm:cc:"+itoa(id)))), ""
	case "cpy":
		toast := "Отмечено как выплаченное"
		changed, err := a.reports.MarkPaid(ctx, admin.TgID, id)
		if err != nil {
			a.log.Error("отметка выплаты", "err", err)
			toast = "Ошибка, подробности в логах"
		} else if !changed {
			toast = "Уже отмечено"
		} else if cc, err := a.reports.CompCardByID(ctx, id); err == nil {
			a.send(ctx, b, cc.User.TgID, i18n.T(lang(cc.User), "comp_paid_notice", fmtMoney(cc.Comp.Amount), esc(cc.Title)), nil)
			a.log.Info("компенсация выплачена", "admin", maskID(admin.TgID), "compensation", id)
		}
		t, m := a.screenComp(ctx, b, admin, id)
		return t, m, toast
	case "rv":
		toast := ""
		if arg(3) == "ok" { // проверено прямо из отчёта
			if changed, err := a.tasks.Review(ctx, admin.TgID, id); err != nil {
				toast = "Ошибка, подробности в логах"
			} else if changed {
				toast = "Отмечено как проверенное"
			}
		}
		t, m := a.screenReport(ctx, b, admin, id)
		return t, m, toast
	}
	status, page := domain.CompPending, 0
	if arg(2) == "d" {
		status = domain.CompPaid
	}
	page, _ = strconv.Atoi(arg(3))
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
	f := "w"
	title := "к выплате"
	if status == domain.CompPaid {
		f, title = "d", "выплачено"
	}
	rows := [][]models.InlineKeyboardButton{row(
		btn(mark("К выплате", status == domain.CompPending), "adm:cp:w:0"),
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
	text := fmt.Sprintf("<b>Компенсации: %s</b>\nЗаписей: %d, на сумму <b>%s сум</b>", title, total, fmtMoney(sum))
	if total == 0 {
		text += "\n\nПока пусто."
	}
	return text, kb(rows...)
}

// screenComp карточка компенсации. Фото чека отправляется отдельным сообщением по file_id.
func (a *App) screenComp(ctx context.Context, b *bot.Bot, admin *domain.User, id int64) (string, *models.InlineKeyboardMarkup) {
	cc, err := a.reports.CompCardByID(ctx, id)
	if err != nil {
		return "Запись не найдена.", kb(row(btn("« К компенсациям", "adm:cp:w:0")))
	}
	c := cc.Comp
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>Компенсация по заданию #%d</b>\n«%s»\nПокупатель: %s\nСумма: <b>%s сум</b>\n", c.TaskID, esc(cc.Title), userLabel(cc.User), fmtMoney(c.Amount))
	if c.Status == domain.CompPaid {
		fmt.Fprintf(&sb, "Статус: ✅ выплачено %s\n", a.fmtTime(c.PaidAt))
	} else {
		sb.WriteString("Статус: ⏳ к выплате\n")
	}
	if cc.DupReceipt != 0 {
		fmt.Fprintf(&sb, "\n⚠ Этот файл чека уже прикладывали в задании #%d. Проверьте, что это не повтор.\n", cc.DupReceipt)
	}
	sb.WriteString("\nФото чека отправлено отдельным сообщением ниже.")

	if _, err := b.SendPhoto(ctx, &bot.SendPhotoParams{
		ChatID: admin.TgID, Photo: &models.InputFileString{Data: c.ReceiptFileID},
		Caption: fmt.Sprintf("Чек, задание #%d, %s сум", c.TaskID, fmtMoney(c.Amount)),
	}); err != nil {
		a.log.Warn("не удалось отправить фото чека", "err", err)
	}
	var rows [][]models.InlineKeyboardButton
	if c.Status == domain.CompPending {
		rows = append(rows, row(btn("💵 Выплачено", "adm:cpd:"+itoa(id))))
	}
	rows = append(rows, row(btn("📄 Отчёт", "adm:rv:"+itoa(c.TaskID)), btn("« К списку", "adm:cp:"+statusLetter(c.Status)+":0")))
	return sb.String(), kb(rows...)
}

func statusLetter(s domain.CompStatus) string {
	if s == domain.CompPaid {
		return "d"
	}
	return "w"
}

// screenReport показывает ответы отчёта; фото и видео отправляются отдельными сообщениями по file_id.
func (a *App) screenReport(ctx context.Context, b *bot.Bot, admin *domain.User, taskID int64) (string, *models.InlineKeyboardMarkup) {
	card, err := a.tasks.Card(ctx, taskID)
	if err != nil {
		return "Задание не найдено.", kb(row(btn("« К заданиям", "adm:tk:a:0")))
	}
	rep, err := a.reports.Get(ctx, taskID)
	if err != nil || rep == nil {
		return "Отчёта по этому заданию пока нет.", kb(row(btn("« К заданию", "adm:tc:"+itoa(taskID))))
	}
	byKey := map[string]domain.Answer{}
	for _, an := range rep.Answers {
		byKey[an.Key] = an
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>Отчёт по заданию #%d</b>\n«%s», %s\nПокупатель: %s\nПолучен: %s", taskID, esc(card.Version.Body.Title), esc(card.Version.Body.Operator), userLabel(card.User), a.fmtTime(rep.SubmittedAt))
	if rep.Late {
		sb.WriteString(" ⏰ после срока")
	}
	sb.WriteString("\n\n")
	var media []domain.Answer
	for i, q := range card.Version.Body.Questions {
		an := byKey[q.Key]
		fmt.Fprintf(&sb, "%d. %s\n   <b>%s</b>\n", i+1, esc(q.Text), esc(a.answerText(i18n.RU, an)))
		if !an.Skipped && (an.Type == domain.QPhoto || an.Type == domain.QVideo) && an.FileID != "" {
			media = append(media, an)
		}
	}
	if card.Comp != nil {
		fmt.Fprintf(&sb, "\n<b>Компенсация:</b> %s сум (%s)\n", fmtMoney(card.Comp.Amount), map[domain.CompStatus]string{domain.CompPending: "к выплате", domain.CompPaid: "выплачено"}[card.Comp.Status])
	}
	if len(media) > 0 {
		sb.WriteString("\nФото и видео отправлены отдельными сообщениями ниже.")
	}

	qText := map[string]string{}
	for i, q := range card.Version.Body.Questions {
		qText[q.Key] = fmt.Sprintf("%d. %s", i+1, cut(q.Text, 200))
	}
	for _, m := range media {
		var err error
		if m.Type == domain.QPhoto {
			_, err = b.SendPhoto(ctx, &bot.SendPhotoParams{ChatID: admin.TgID, Photo: &models.InputFileString{Data: m.FileID}, Caption: qText[m.Key]})
		} else {
			_, err = b.SendVideo(ctx, &bot.SendVideoParams{ChatID: admin.TgID, Video: &models.InputFileString{Data: m.FileID}, Caption: qText[m.Key]})
		}
		if err != nil {
			a.log.Warn("не удалось отправить медиа отчёта", "err", err)
		}
	}

	var rows [][]models.InlineKeyboardButton
	if card.Task.Status == domain.TaskReported {
		rows = append(rows, row(btn("✅ Проверено", "adm:rv:"+itoa(taskID)+":ok")))
	}
	if card.Comp != nil {
		rows = append(rows, row(btn("💰 Компенсация", "adm:cc:"+itoa(card.Comp.ID))))
	}
	rows = append(rows, row(btn("« К заданию", "adm:tc:"+itoa(taskID))))
	return sb.String(), kb(rows...)
}
