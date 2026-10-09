package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"botech/internal/domain"
	"botech/internal/i18n"
	"botech/internal/service"
)

// Пошаговый отчёт. Состояние хранится в БД (fsm_states), поэтому черновик переживает рестарт.

const reportDialog = "report"

// Фазы диалога.
const (
	phaseQuestion    = "q"
	phaseCompAsk     = "comp_ask"
	phaseCompAmount  = "comp_amount"
	phaseCompReceipt = "comp_receipt"
	phaseConfirm     = "confirm"
)

// loadReport читает состояние; ok=false, если активного отчёта нет.
func (a *App) loadReport(ctx context.Context, userID int64) (*reportState, bool) {
	var st reportState
	name, err := a.dialog.Get(ctx, userID, &st)
	if err != nil {
		a.log.Error("чтение состояния отчёта", "err", err)
		return nil, false
	}
	return &st, name == reportDialog
}

func (a *App) saveReport(ctx context.Context, userID int64, st *reportState) {
	if err := a.dialog.Set(ctx, userID, reportDialog, st); err != nil {
		a.log.Error("сохранение состояния отчёта", "err", err)
	}
}

// startReport начинает отчёт или продолжает черновик по тому же заданию.
func (a *App) startReport(ctx context.Context, b *bot.Bot, u *domain.User, cb *models.CallbackQuery, taskID int64) {
	l := lang(u)
	if _, err := a.reports.CanReport(ctx, u.TgID, taskID); err != nil {
		switch {
		case errors.Is(err, service.ErrAlreadyReported):
			a.answerCB(ctx, b, cb.ID, i18n.T(l, "rpt_already"), true)
		case errors.Is(err, service.ErrForbidden):
			a.answerCB(ctx, b, cb.ID, errText(err), true)
		default:
			a.answerCB(ctx, b, cb.ID, i18n.T(l, "task_not_found"), true)
		}
		return
	}
	a.answerCB(ctx, b, cb.ID, "", false)
	st, ok := a.loadReport(ctx, u.TgID)
	if !ok || st.TaskID != taskID || st.CompOnly {
		st = &reportState{TaskID: taskID, Phase: phaseQuestion}
		if card, err := a.tasks.Card(ctx, taskID); err == nil && card.Version.Body.Kind == domain.KindSeller {
			st.NoComp = true // у продавца нет компенсации
		}
		// После возврата на доработку сначала напоминаем, что просил исправить админ.
		if card, err := a.tasks.Card(ctx, taskID); err == nil {
			if card.Task.Status == domain.TaskRework && card.Report != nil && card.Report.AdminComment != "" {
				a.send(ctx, b, u.TgID, i18n.T(l, "rpt_rework_intro", esc(card.Report.AdminComment)), nil)
			}
			if c := card.Comp; c != nil {
				if c.Status == domain.CompRejected {
					st.CompRejected = true // данные были отклонены, без новых не обойтись
				} else {
					st.KeepComp, st.KeepAmount = true, c.Amount
				}
			}
		}
	}
	st.Step++
	a.saveReport(ctx, u.TgID, st)
	a.ask(ctx, b, u, st)
}

// startCompFix запускает короткий диалог: только сумма и чек для отклонённой компенсации.
func (a *App) startCompFix(ctx context.Context, b *bot.Bot, u *domain.User, cb *models.CallbackQuery, taskID int64) {
	l := lang(u)
	card, err := a.tasks.Card(ctx, taskID)
	if err != nil || card.Task.UserID != u.TgID || card.Comp == nil {
		a.answerCB(ctx, b, cb.ID, i18n.T(l, "task_not_found"), true)
		return
	}
	if card.Comp.Status != domain.CompRejected {
		a.answerCB(ctx, b, cb.ID, i18n.T(l, "task_already"), true)
		return
	}
	a.answerCB(ctx, b, cb.ID, "", false)
	st, ok := a.loadReport(ctx, u.TgID)
	if !ok || st.TaskID != taskID || !st.CompOnly {
		st = &reportState{TaskID: taskID, CompOnly: true, WantComp: true, Phase: phaseCompAmount}
	}
	st.Step++
	a.saveReport(ctx, u.TgID, st)
	a.send(ctx, b, u.TgID, i18n.T(l, "comp_rejected", esc(card.Comp.AdminComment)), nil)
	a.ask(ctx, b, u, st)
}

// ask показывает вопрос текущей фазы.
func (a *App) ask(ctx context.Context, b *bot.Bot, u *domain.User, st *reportState) {
	l := lang(u)
	card, err := a.tasks.Card(ctx, st.TaskID)
	if err != nil {
		a.log.Error("карточка для отчёта", "err", err)
		return
	}
	qs := card.Version.Body.Questions
	step := strconv.Itoa(st.Step)
	cb := func(act string, extra ...string) string {
		return strings.Join(append([]string{"rpt", act, step}, extra...), ":")
	}
	nav := func(canBack, canSkip bool) []models.InlineKeyboardButton {
		var r []models.InlineKeyboardButton
		if canBack {
			r = append(r, btn(i18n.T(l, "btn_back"), cb("b")))
		}
		if canSkip {
			r = append(r, btn(i18n.T(l, "btn_skip"), cb("s")))
		}
		return append(r, btn(i18n.T(l, "btn_cancel"), cb("x")))
	}

	switch st.Phase {
	case phaseQuestion:
		q := qs[st.Idx]
		text := i18n.T(l, "rpt_question", st.Idx+1, len(qs), esc(q.Text)) + "\n\n"
		var rows [][]models.InlineKeyboardButton
		switch q.Type {
		case domain.QRating:
			text += i18n.T(l, "rpt_hint_rating")
			var r []models.InlineKeyboardButton
			for i := 1; i <= 5; i++ {
				r = append(r, btn(strconv.Itoa(i), cb("r", strconv.Itoa(i))))
			}
			rows = append(rows, r)
		case domain.QYesNo:
			text += i18n.T(l, "rpt_hint_yesno")
			rows = append(rows, row(btn(i18n.T(l, "btn_yes"), cb("y", "yes")), btn(i18n.T(l, "btn_no_short"), cb("y", "no"))))
		case domain.QText:
			text += i18n.T(l, "rpt_hint_text")
		case domain.QPhoto:
			text += i18n.T(l, "rpt_hint_photo")
		case domain.QVideo:
			text += i18n.T(l, "rpt_hint_video")
		}
		if !q.Required {
			text += i18n.T(l, "rpt_optional")
		}
		rows = append(rows, nav(st.Idx > 0, !q.Required))
		a.sendPanel(ctx, b, u.TgID, text, kb(rows...))
	case phaseCompAsk:
		a.sendPanel(ctx, b, u.TgID, i18n.T(l, "rpt_comp_ask"), kb(
			row(btn(i18n.T(l, "btn_comp_yes"), cb("ca", "1")), btn(i18n.T(l, "btn_comp_no"), cb("ca", "0"))),
			nav(true, false)))
	case phaseCompAmount:
		a.sendPanel(ctx, b, u.TgID, i18n.T(l, "rpt_amount_ask"), kb(nav(!st.CompOnly, false)))
	case phaseCompReceipt:
		a.sendPanel(ctx, b, u.TgID, i18n.T(l, "rpt_receipt_ask"), kb(nav(true, false)))
	case phaseConfirm:
		if st.CompOnly {
			a.sendPanel(ctx, b, u.TgID, i18n.T(l, "comp_fix_confirm", fmtMoney(st.Amount)), kb(
				row(btn(i18n.T(l, "btn_send_fix"), cb("ok"))), nav(true, false)))
			break
		}
		rows := [][]models.InlineKeyboardButton{row(btn(i18n.T(l, "btn_send_report"), cb("ok")))}
		if st.KeepComp && !st.WantComp {
			rows = append(rows, row(btn(i18n.T(l, "btn_change_comp"), cb("cc"))))
		}
		a.sendPanel(ctx, b, u.TgID, a.reportSummary(l, qs, st), kb(append(rows, nav(true, false))...))
	}
}

// reportSummary итоговый текст перед отправкой.
func (a *App) reportSummary(l i18n.Lang, qs []domain.Question, st *reportState) string {
	byKey := map[string]domain.Answer{}
	for _, an := range st.Answers {
		byKey[an.Key] = an
	}
	var sb strings.Builder
	sb.WriteString(i18n.T(l, "rpt_confirm_head"))
	for i, q := range qs {
		fmt.Fprintf(&sb, "%d. %s: <b>%s</b>\n", i+1, esc(cut(q.Text, 80)), esc(a.answerText(l, byKey[q.Key])))
	}
	if st.NoComp {
		// у продавца компенсации нет, строку про неё не показываем
	} else if st.WantComp {
		sb.WriteString(i18n.T(l, "rpt_comp_line", fmtMoney(st.Amount)))
	} else if st.KeepComp {
		sb.WriteString(i18n.T(l, "rpt_comp_keep", fmtMoney(st.KeepAmount)))
	} else {
		sb.WriteString(i18n.T(l, "rpt_comp_none"))
	}
	sb.WriteString(i18n.T(l, "rpt_confirm_footer"))
	return sb.String()
}

func (a *App) answerText(l i18n.Lang, an domain.Answer) string {
	switch {
	case an.Skipped || an.Key == "":
		return i18n.T(l, "rpt_answer_skipped")
	case an.Type == domain.QPhoto || an.Type == domain.QVideo:
		return i18n.T(l, "rpt_answer_file")
	case an.Type == domain.QYesNo && an.Value == "yes":
		return i18n.T(l, "rpt_yes")
	case an.Type == domain.QYesNo:
		return i18n.T(l, "rpt_no")
	}
	return an.Value
}

// onReportCallback кнопки мастера: rpt:<действие>:<шаг>[:<аргумент>].
func (a *App) onReportCallback(ctx context.Context, b *bot.Bot, upd *models.Update) {
	cb := upd.CallbackQuery
	u := userFrom(ctx)
	if u == nil {
		return
	}
	l := lang(u)
	parts := strings.Split(cb.Data, ":")
	if len(parts) < 3 {
		return
	}
	st, ok := a.loadReport(ctx, u.TgID)
	if !ok {
		a.answerCB(ctx, b, cb.ID, i18n.T(l, "rpt_no_draft"), true)
		return
	}
	if step, _ := strconv.Atoi(parts[2]); step != st.Step {
		a.answerCB(ctx, b, cb.ID, i18n.T(l, "rpt_stale"), true)
		return
	}
	card, err := a.tasks.Card(ctx, st.TaskID)
	if err != nil || card.Task.UserID != u.TgID {
		a.answerCB(ctx, b, cb.ID, i18n.T(l, "task_not_found"), true)
		return
	}
	qs := card.Version.Body.Questions
	arg := ""
	if len(parts) > 3 {
		arg = parts[3]
	}

	switch parts[1] {
	case "x":
		_ = a.dialog.Clear(ctx, u.TgID)
		a.answerCB(ctx, b, cb.ID, "", false)
		a.edit(ctx, b, cb, i18n.T(l, "rpt_cancelled"), nil)
		return
	case "b":
		st.back(len(qs))
	case "s":
		if st.Phase != phaseQuestion || qs[st.Idx].Required {
			a.answerCB(ctx, b, cb.ID, i18n.T(l, "rpt_stale"), true)
			return
		}
		q := qs[st.Idx]
		st.setAnswer(domain.Answer{Key: q.Key, Type: q.Type, Skipped: true})
		st.advance(len(qs))
	case "r", "y":
		if st.Phase != phaseQuestion {
			a.answerCB(ctx, b, cb.ID, i18n.T(l, "rpt_stale"), true)
			return
		}
		q := qs[st.Idx]
		if (parts[1] == "r") != (q.Type == domain.QRating) || (parts[1] == "y") != (q.Type == domain.QYesNo) {
			a.answerCB(ctx, b, cb.ID, i18n.T(l, "rpt_stale"), true)
			return
		}
		st.setAnswer(domain.Answer{Key: q.Key, Type: q.Type, Value: arg})
		st.advance(len(qs))
	case "ca":
		if st.Phase != phaseCompAsk {
			a.answerCB(ctx, b, cb.ID, i18n.T(l, "rpt_stale"), true)
			return
		}
		st.WantComp = arg == "1"
		st.advance(len(qs))
	case "cc": // изменить уже отправленную компенсацию
		if st.Phase != phaseConfirm || !st.KeepComp || st.WantComp {
			a.answerCB(ctx, b, cb.ID, i18n.T(l, "rpt_stale"), true)
			return
		}
		st.WantComp, st.Phase = true, phaseCompAmount
	case "ok":
		if st.Phase != phaseConfirm {
			a.answerCB(ctx, b, cb.ID, i18n.T(l, "rpt_stale"), true)
			return
		}
		a.answerCB(ctx, b, cb.ID, "", false)
		a.removeButtons(ctx, b, cb)
		a.submitReport(ctx, b, u, st)
		return
	default:
		return
	}
	a.answerCB(ctx, b, cb.ID, "", false)
	a.removeButtons(ctx, b, cb)
	st.Step++
	a.saveReport(ctx, u.TgID, st)
	a.ask(ctx, b, u, st)
}

// removeButtons убирает кнопки у обработанного сообщения, чтобы на них нельзя было нажать повторно.
func (a *App) removeButtons(ctx context.Context, b *bot.Bot, cb *models.CallbackQuery) {
	if msg := cb.Message.Message; msg != nil {
		_, _ = b.EditMessageReplyMarkup(ctx, &bot.EditMessageReplyMarkupParams{
			ChatID: msg.Chat.ID, MessageID: msg.ID, ReplyMarkup: models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}},
		})
	}
}

// onReportMessage обрабатывает текст, фото и видео покупателя во время отчёта.
func (a *App) onReportMessage(ctx context.Context, b *bot.Bot, u *domain.User, st *reportState, m *models.Message) {
	l := lang(u)
	card, err := a.tasks.Card(ctx, st.TaskID)
	if err != nil || card.Task.UserID != u.TgID {
		_ = a.dialog.Clear(ctx, u.TgID)
		return
	}
	qs := card.Version.Body.Questions
	remind := func(key string) { a.send(ctx, b, u.TgID, i18n.T(l, key), nil) }

	switch st.Phase {
	case phaseQuestion:
		q := qs[st.Idx]
		an := domain.Answer{Key: q.Key, Type: q.Type}
		switch q.Type {
		case domain.QText:
			text := strings.TrimSpace(m.Text)
			if text == "" {
				remind("rpt_wait_text")
				return
			}
			if utf8.RuneCountInString(text) > service.MaxTextAnswer {
				a.send(ctx, b, u.TgID, i18n.T(l, "rpt_text_long", service.MaxTextAnswer), nil)
				return
			}
			an.Value = text
		case domain.QPhoto:
			id, uid := largestPhoto(m)
			if id == "" {
				remind("rpt_wait_photo")
				return
			}
			an.FileID, an.FileUniqueID = id, uid
		case domain.QVideo:
			if m.Video == nil {
				remind("rpt_wait_video")
				return
			}
			an.FileID, an.FileUniqueID = m.Video.FileID, m.Video.FileUniqueID
		default: // оценка и да/нет выбираются кнопками
			remind("rpt_wait_choice")
			return
		}
		st.setAnswer(an)
	case phaseCompAmount:
		amount, ok := parseAmount(m.Text)
		if !ok {
			a.send(ctx, b, u.TgID, i18n.T(l, "rpt_amount_bad", service.MaxCompAmount), nil)
			return
		}
		st.Amount = amount
	case phaseCompReceipt:
		id, uid := largestPhoto(m)
		if id == "" {
			remind("rpt_wait_photo")
			return
		}
		st.ReceiptFileID, st.ReceiptUniqueID = id, uid
	default: // подтверждение и вопрос о компенсации: ждём кнопку
		remind("rpt_wait_choice")
		return
	}
	if len(st.Msgs) < 100 {
		st.Msgs = append(st.Msgs, m.ID)
	}
	st.advance(len(qs))
	st.Step++
	a.saveReport(ctx, u.TgID, st)
	a.ask(ctx, b, u, st)
}
