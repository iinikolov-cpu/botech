package handlers

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"botech/internal/domain"
	"botech/internal/service"
	"botech/internal/storage"
)

// Статистика и выгрузки: adm:sx:<период>:<b|s> (сводка по покупателям или продавцам),
// adm:ex:<период> (меню выгрузки), adm:exd:<период>:<t|r|c|a> (отправка файла).
// Период: w 7 дней, m 30 дней, a всё время.

func (a *App) statsCallback(ctx context.Context, b *bot.Bot, admin *domain.User, parts []string) (string, *models.InlineKeyboardMarkup, string) {
	period := "m"
	if len(parts) > 2 && (parts[2] == "w" || parts[2] == "m" || parts[2] == "a") {
		period = parts[2]
	}
	switch parts[1] {
	case "ex":
		t, m := a.screenExport(period)
		return t, m, ""
	case "exd":
		toast := a.sendExport(ctx, b, admin, period, parts[3:])
		if toast == "" {
			toast = "Файл отправлен"
		}
		t, m := a.screenExport(period)
		return t, m, toast
	}
	kind := domain.KindBuyer
	if len(parts) > 3 && parts[3] == "s" {
		kind = domain.KindSeller
	}
	t, m := a.screenStats(ctx, period, kind)
	return t, m, ""
}

func (a *App) periodFilter(period string) (storage.ExportFilter, string) {
	from, to, title := service.Period(period, time.Now())
	return storage.ExportFilter{From: from, To: to}, title
}

func kindLetter(k domain.TaskKind) string {
	if k == domain.KindSeller {
		return "s"
	}
	return "b"
}

func (a *App) screenStats(ctx context.Context, period string, kind domain.TaskKind) (string, *models.InlineKeyboardMarkup) {
	f, title := a.periodFilter(period)
	st, err := a.analytics.Stats(ctx, f, kind)
	if err != nil {
		a.log.Error("статистика", "err", err)
		return "Не удалось посчитать статистику, подробности в логах.", kb(row(btn("« Назад", "adm:home")))
	}
	who := "покупатели"
	if kind == domain.KindSeller {
		who = "продавцы"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>Статистика: %s</b>, период: %s\n\n", who, title)
	if st.Total == 0 {
		sb.WriteString("За этот период заданий этого типа нет.")
	} else {
		fmt.Fprintf(&sb, "Заданий: <b>%d</b>, доставлено: %d, принято: %d\n", st.Total, st.Issued, st.Accepted)
		fmt.Fprintf(&sb, "Отказались: %d, отменено: %d\n", st.Declined, st.Cancelled)
		fmt.Fprintf(&sb, "Ждут ответа: %d, в работе: %d, просрочено: %d\n", st.Waiting, st.InWork, st.Overdue)
		fmt.Fprintf(&sb, "Отчётов получено: <b>%d</b>, проверено: %d\n", st.Completed, st.Reviewed)
		fmt.Fprintf(&sb, "Среднее время от принятия до отчёта: %s\n", service.FormatDuration(st.AvgTime))
		if kind == domain.KindBuyer {
			fmt.Fprintf(&sb, "\nТоваров выбрано: %d, куплено: %d\n", st.ItemsChosen, st.ItemsBought)
			fmt.Fprintf(&sb, "\n<b>Компенсации</b>\nК выплате: %d (%s сум), выплачено: %d (%s сум), отклонено: %d\n",
				st.CompPending, fmtMoney(st.SumPending), st.CompPaid, fmtMoney(st.SumPaid), st.CompRejected)
		}
		if len(st.Operators) > 0 {
			sb.WriteString("\n<b>По операторам</b>\n")
		}
		for _, op := range st.Operators {
			fmt.Fprintf(&sb, "• %s: выдано %d, отчётов %d, просрочено %d, среднее время: %s\n",
				esc(op.Operator), op.Issued, op.Completed, op.Overdue, service.FormatDuration(op.AvgTime))
		}
	}
	tab := func(label string, k domain.TaskKind) models.InlineKeyboardButton {
		return btn(mark(label, k == kind), "adm:sx:"+period+":"+kindLetter(k))
	}
	periodTab := func(label, p string) models.InlineKeyboardButton {
		return btn(mark(label, p == period), "adm:sx:"+p+":"+kindLetter(kind))
	}
	return cut(sb.String(), 4000), kb(
		row(tab("Покупатели", domain.KindBuyer), tab("Продавцы", domain.KindSeller)),
		row(periodTab("7 дней", "w"), periodTab("30 дней", "m"), periodTab("Всё время", "a")),
		row(btn("📤 Выгрузить в CSV", "adm:ex:"+period)),
		row(btn("« Назад", "adm:home")),
	)
}

func (a *App) screenExport(period string) (string, *models.InlineKeyboardMarkup) {
	_, title := a.periodFilter(period)
	text := fmt.Sprintf("<b>Выгрузка в CSV</b>, период: %s\n\n"+
		"Файлы открываются в Excel и Google Таблицах.\n"+
		"• Отчёты: одна строка на отчёт, сразу по всем сценариям. Есть роль (покупатель или продавец), Telegram ID, колонка на каждый вопрос каждого сценария. "+
		"Вместо фото и видео в ячейке ссылка: админ открывает её в Telegram, и бот присылает файл.\n"+
		"• Задания: все задания, даты этапов, напоминания, айтем, промокод, компенсация.\n"+
		"• Компенсации: суммы и статусы.\n"+
		"• Журнал админов: последние 5000 действий (без учёта периода).", title)
	return text, kb(
		row(btn(mark("7 дней", period == "w"), "adm:ex:w"), btn(mark("30 дней", period == "m"), "adm:ex:m"), btn(mark("Всё время", period == "a"), "adm:ex:a")),
		row(btn("📝 Отчёты", "adm:exd:"+period+":r"), btn("📌 Задания", "adm:exd:"+period+":t")),
		row(btn("💰 Компенсации", "adm:exd:"+period+":c"), btn("📜 Журнал админов", "adm:exd:"+period+":a")),
		row(btn("« К статистике", "adm:sx:"+period+":b")),
	)
}

// sendExport формирует и отправляет файл. Возвращает текст ошибки для всплывающего окна или "".
func (a *App) sendExport(ctx context.Context, b *bot.Bot, admin *domain.User, period string, args []string) string {
	if len(args) == 0 {
		return "Неизвестный тип выгрузки"
	}
	f, _ := a.periodFilter(period)
	day := time.Now().In(a.loc).Format("2006-01-02")
	var (
		data []byte
		name string
		err  error
	)
	switch args[0] {
	case "t", "c", "r":
		rows, e := a.analytics.Rows(ctx, f)
		if e != nil {
			err = e
			break
		}
		switch args[0] {
		case "t":
			data, name = a.analytics.TasksCSV(rows), "tasks-"+day+".csv"
		case "c":
			data, name = a.analytics.CompsCSV(rows), "compensations-"+day+".csv"
		default:
			data, err = a.analytics.AnswersCSV(ctx, rows)
			name = "reports-" + day + ".csv"
		}
	case "a":
		data, err = a.analytics.AuditCSV(ctx, 5000)
		name = "audit-" + day + ".csv"
	default:
		return "Неизвестный тип выгрузки"
	}
	if err != nil {
		a.log.Error("выгрузка CSV", "err", err)
		return "Не удалось сформировать файл, подробности в логах"
	}
	if len(data) > service.MaxTelegramFile {
		return "Файл слишком большой для Telegram, выберите период короче"
	}
	_, err = b.SendDocument(ctx, &bot.SendDocumentParams{
		ChatID:   admin.TgID,
		Document: &models.InputFileUpload{Filename: name, Data: bytes.NewReader(data)},
		Caption:  "📤 " + name,
	})
	if err != nil {
		a.log.Error("отправка CSV", "err", err)
		return "Не удалось отправить файл"
	}
	a.log.Info("выгрузка отправлена", "admin", maskID(admin.TgID), "file", args[0])
	return ""
}
