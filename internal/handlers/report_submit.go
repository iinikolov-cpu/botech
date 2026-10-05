package handlers

import (
	"botech/internal/domain"
	"botech/internal/i18n"
	"botech/internal/service"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

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
	a.sendPanel(ctx, b, u.TgID, text, kb(row(btn(i18n.T(l, "btn_tasks"), "tsk:l"))))
	a.deleteMsgs(ctx, b, u.TgID, st.Msgs)
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
		a.notifyAdminsKeyed(ctx, b, "promo_warn", "⚠ Не осталось ни одного промокода со свободными использованиями. Загрузите новые коды.", kb(row(btn("🎁 Промокоды", "adm:pr"))))
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
	a.sendPanel(ctx, b, u.TgID, i18n.T(l, "comp_fix_sent"), kb(row(btn(i18n.T(l, "btn_tasks"), "tsk:l"))))
	a.deleteMsgs(ctx, b, u.TgID, st.Msgs)
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
