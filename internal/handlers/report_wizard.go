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

type reportState struct {
	TaskID  int64           `json:"task_id"`
	Step    int             `json:"step"` // растёт при каждом изменении: защита от нажатий на устаревшие кнопки
	Phase   string          `json:"phase"`
	Idx     int             `json:"idx"` // номер вопроса в фазе phaseQuestion
	Answers []domain.Answer `json:"answers"`

	WantComp bool `json:"want_comp"`
	CompOnly bool `json:"comp_only"` // исправление только данных компенсации, без отчёта
	// При доработке отчёта уже отправленную компенсацию повторно не запрашиваем.
	KeepComp        bool   `json:"keep_comp"`     // данные компенсации уже есть (к выплате или выплачена) и остаются как есть
	KeepAmount      int64  `json:"keep_amount"`   // сумма уже отправленной компенсации (для показа)
	CompRejected    bool   `json:"comp_rejected"` // компенсация отклонена: новые данные обязательны
	Amount          int64  `json:"amount"`
	ReceiptFileID   string `json:"receipt_file_id"`
	ReceiptUniqueID string `json:"receipt_unique_id"`
}

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
		a.send(ctx, b, u.TgID, text, kb(rows...))
	case phaseCompAsk:
		a.send(ctx, b, u.TgID, i18n.T(l, "rpt_comp_ask"), kb(
			row(btn(i18n.T(l, "btn_comp_yes"), cb("ca", "1")), btn(i18n.T(l, "btn_comp_no"), cb("ca", "0"))),
			nav(true, false)))
	case phaseCompAmount:
		a.send(ctx, b, u.TgID, i18n.T(l, "rpt_amount_ask"), kb(nav(!st.CompOnly, false)))
	case phaseCompReceipt:
		a.send(ctx, b, u.TgID, i18n.T(l, "rpt_receipt_ask"), kb(nav(true, false)))
	case phaseConfirm:
		if st.CompOnly {
			a.send(ctx, b, u.TgID, i18n.T(l, "comp_fix_confirm", fmtMoney(st.Amount)), kb(
				row(btn(i18n.T(l, "btn_send_fix"), cb("ok"))), nav(true, false)))
			break
		}
		rows := [][]models.InlineKeyboardButton{row(btn(i18n.T(l, "btn_send_report"), cb("ok")))}
		if st.KeepComp && !st.WantComp {
			rows = append(rows, row(btn(i18n.T(l, "btn_change_comp"), cb("cc"))))
		}
		a.send(ctx, b, u.TgID, a.reportSummary(l, qs, st), kb(append(rows, nav(true, false))...))
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
	if st.WantComp {
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

// setAnswer записывает (или заменяет) ответ на вопрос.
func (st *reportState) setAnswer(an domain.Answer) {
	for i := range st.Answers {
		if st.Answers[i].Key == an.Key {
			st.Answers[i] = an
			return
		}
	}
	st.Answers = append(st.Answers, an)
}

// advance переходит к следующей фазе после ответа на вопрос.
func (st *reportState) advance(total int) {
	switch st.Phase {
	case phaseQuestion:
		switch {
		case st.Idx+1 < total:
			st.Idx++
		case st.KeepComp: // компенсация уже отправлена: сразу к итогу
			st.Phase = phaseConfirm
		case st.CompRejected: // прежние данные отклонены: спрашивать «нужна ли» незачем
			st.WantComp, st.Phase = true, phaseCompAmount
		default:
			st.Phase = phaseCompAsk
		}
	case phaseCompAsk:
		if st.WantComp {
			st.Phase = phaseCompAmount
		} else {
			st.Phase = phaseConfirm
		}
	case phaseCompAmount:
		st.Phase = phaseCompReceipt
	case phaseCompReceipt:
		st.Phase = phaseConfirm
	}
}

// back возвращается на шаг назад и стирает ответ, к которому вернулись.
func (st *reportState) back(total int) {
	switch st.Phase {
	case phaseQuestion:
		if st.Idx > 0 {
			st.Idx--
		}
	case phaseCompAsk:
		st.Phase, st.Idx = phaseQuestion, total-1
	case phaseCompAmount:
		switch {
		case st.CompOnly: // при исправлении компенсации шага «назад» нет
		case st.KeepComp: // передумал менять: возвращаемся к итогу с прежней компенсацией
			st.WantComp, st.Phase = false, phaseConfirm
		case st.CompRejected:
			st.Phase, st.Idx = phaseQuestion, total-1
		default:
			st.Phase = phaseCompAsk
		}
	case phaseCompReceipt:
		st.Phase = phaseCompAmount
	case phaseConfirm:
		switch {
		case st.WantComp:
			st.Phase = phaseCompReceipt
		case st.KeepComp:
			st.Phase, st.Idx = phaseQuestion, total-1
		default:
			st.Phase = phaseCompAsk
		}
	}
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
	st.advance(len(qs))
	st.Step++
	a.saveReport(ctx, u.TgID, st)
	a.ask(ctx, b, u, st)
}

// submitReport отправляет готовый отчёт и уведомляет админа.
func (a *App) submitReport(ctx context.Context, b *bot.Bot, u *domain.User, st *reportState) {
	l := lang(u)
	if st.CompOnly {
		a.submitCompFix(ctx, b, u, st)
		return
	}
	in := service.SubmitInput{Answers: st.Answers}
	if st.WantComp {
		in.Comp = &service.CompInput{Amount: st.Amount, ReceiptFileID: st.ReceiptFileID, ReceiptUniqueID: st.ReceiptUniqueID}
	}
	res, err := a.reports.Submit(ctx, u.TgID, st.TaskID, in)
	switch {
	case errors.Is(err, service.ErrAlreadyReported):
		_ = a.dialog.Clear(ctx, u.TgID)
		a.send(ctx, b, u.TgID, i18n.T(l, "rpt_already"), nil)
		return
	case errors.Is(err, service.ErrInvalidReport), errors.Is(err, service.ErrForbidden), errors.Is(err, service.ErrNotFound):
		a.send(ctx, b, u.TgID, i18n.T(l, "rpt_error", esc(errText(err))), nil)
		return
	case err != nil:
		a.log.Error("отправка отчёта", "err", err)
		a.send(ctx, b, u.TgID, i18n.T(l, "rpt_error", "попробуйте ещё раз"), nil)
		return
	}
	_ = a.dialog.Clear(ctx, u.TgID)
	text := i18n.T(l, "rpt_sent")
	if res.Late {
		text += i18n.T(l, "rpt_sent_late")
	}
	a.send(ctx, b, u.TgID, text, kb(row(btn(i18n.T(l, "btn_tasks"), "tsk:l"))))
	a.log.Info("отчёт получен", "user", maskID(u.TgID), "task", st.TaskID, "late", res.Late)

	msg := fmt.Sprintf("📝 Получен отчёт от %s по заданию #%d «%s»", userLabel(u), st.TaskID, esc(res.Card.Version.Body.Title))
	if res.Late {
		msg += "\n⏰ Отправлен после срока."
	}
	if res.Card.Comp != nil {
		msg += fmt.Sprintf("\n💰 К компенсации: %s сум.", fmtMoney(res.Card.Comp.Amount))
	}
	if res.DupReceipt != 0 {
		msg += fmt.Sprintf("\n⚠ Этот файл чека уже прикладывали в задании #%d. Проверьте.", res.DupReceipt)
	}
	a.notifyAdmins(ctx, b, msg, kb(row(btn("📄 Открыть отчёт", "adm:rv:"+itoa(st.TaskID)))))
	if res.PoolExhausted {
		a.notifyAdmins(ctx, b, "⚠ Не осталось ни одного промокода со свободными использованиями. Загрузите новые коды.", kb(row(btn("🎁 Промокоды", "adm:pr"))))
	}
}

// largestPhoto возвращает file_id и file_unique_id самого крупного варианта фото.
func largestPhoto(m *models.Message) (string, string) {
	if len(m.Photo) == 0 {
		return "", ""
	}
	p := m.Photo[len(m.Photo)-1] // Telegram присылает размеры по возрастанию
	return p.FileID, p.FileUniqueID
}

// parseAmount разбирает сумму в сумах. Пробелы и разделители тысяч (150 000, 150.000, 150,000)
// допускаются, а дробная часть (1500.50) нет: сумы считаем целыми.
func parseAmount(s string) (int64, bool) {
	s = strings.NewReplacer(" ", "", "\u00a0", "").Replace(strings.TrimSpace(s))
	groups := strings.FieldsFunc(s, func(r rune) bool { return r == '.' || r == ',' })
	if len(groups) == 0 || strings.HasPrefix(s, ".") || strings.HasPrefix(s, ",") {
		return 0, false
	}
	var digits strings.Builder
	for i, g := range groups {
		if i > 0 && len(g) != 3 { // после разделителя должна идти группа ровно из трёх цифр
			return 0, false
		}
		for _, r := range g {
			if r < '0' || r > '9' {
				return 0, false
			}
		}
		digits.WriteString(g)
	}
	if digits.Len() > 12 {
		return 0, false
	}
	v, err := strconv.ParseInt(digits.String(), 10, 64)
	if err != nil || v < 1 || v > service.MaxCompAmount {
		return 0, false
	}
	return v, true
}

// fmtMoney форматирует сумму с пробелами между тысячами: 150 000.
func fmtMoney(v int64) string {
	s := strconv.FormatInt(v, 10)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ' ')
		}
		out = append(out, c)
	}
	return string(out)
}

// submitCompFix отправляет исправленные данные компенсации и уведомляет админов.
func (a *App) submitCompFix(ctx context.Context, b *bot.Bot, u *domain.User, st *reportState) {
	l := lang(u)
	dup, err := a.reports.ResubmitCompensation(ctx, u.TgID, st.TaskID, service.CompInput{
		Amount: st.Amount, ReceiptFileID: st.ReceiptFileID, ReceiptUniqueID: st.ReceiptUniqueID,
	})
	if err != nil {
		if errors.Is(err, service.ErrForbidden) || errors.Is(err, service.ErrNotFound) {
			_ = a.dialog.Clear(ctx, u.TgID) // исправлять уже нечего
		} else if !errors.Is(err, service.ErrInvalidReport) {
			a.log.Error("исправление компенсации", "err", err)
		}
		a.send(ctx, b, u.TgID, i18n.T(l, "comp_fix_error", esc(errText(err))), nil)
		return
	}
	_ = a.dialog.Clear(ctx, u.TgID)
	a.send(ctx, b, u.TgID, i18n.T(l, "comp_fix_sent"), kb(row(btn(i18n.T(l, "btn_tasks"), "tsk:l"))))
	a.log.Info("компенсация исправлена", "user", maskID(u.TgID), "task", st.TaskID)

	card, err := a.tasks.Card(ctx, st.TaskID)
	if err != nil || card.Comp == nil {
		return
	}
	msg := fmt.Sprintf("💰 %s исправил данные компенсации по заданию #%d: %s сум.", userLabel(u), st.TaskID, fmtMoney(card.Comp.Amount))
	if dup != 0 {
		msg += fmt.Sprintf("\n⚠ Этот файл чека уже прикладывали в задании #%d. Проверьте.", dup)
	}
	a.notifyAdmins(ctx, b, msg, kb(row(btn("💰 Открыть", "adm:cc:"+itoa(card.Comp.ID)))))
}
