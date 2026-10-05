package handlers

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"botech/internal/domain"
	"botech/internal/service"
	"botech/internal/storage"
)

// Статистика и выгрузки: adm:sx:<период> (сводка), adm:ex:<период> (меню выгрузки),
// adm:exs:<период> (выбор сценария), adm:exd:<период>:<t|c|a|s>[:<id сценария>] (отправка файла).
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
	case "exs":
		t, m := a.screenExportScenarios(ctx, period)
		return t, m, ""
	case "exd":
		toast := a.sendExport(ctx, b, admin, period, parts[3:])
		if toast == "" {
			toast = "Файл отправлен"
		}
		t, m := a.screenExport(period)
		return t, m, toast
	}
	t, m := a.screenStats(ctx, period)
	return t, m, ""
}

func (a *App) periodFilter(period string) (storage.ExportFilter, string) {
	from, to, title := service.Period(period, time.Now())
	return storage.ExportFilter{From: from, To: to}, title
}

func periodTabs(prefix, period string) []models.InlineKeyboardButton {
	tab := func(title, p string) models.InlineKeyboardButton {
		return btn(mark(title, p == period), prefix+p)
	}
	return row(tab("7 дней", "w"), tab("30 дней", "m"), tab("Всё время", "a"))
}

func (a *App) screenStats(ctx context.Context, period string) (string, *models.InlineKeyboardMarkup) {
	f, title := a.periodFilter(period)
	st, err := a.analytics.Stats(ctx, f)
	if err != nil {
		a.log.Error("статистика", "err", err)
		return "Не удалось посчитать статистику, подробности в логах.", kb(row(btn("« Назад", "adm:home")))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>Статистика</b>, период: %s\n\n", title)
	if st.Total == 0 {
		sb.WriteString("За этот период заданий нет.")
	} else {
		fmt.Fprintf(&sb, "Заданий: <b>%d</b>, доставлено: %d, принято: %d\n", st.Total, st.Issued, st.Accepted)
		fmt.Fprintf(&sb, "Отказались: %d, отменено: %d\n", st.Declined, st.Cancelled)
		fmt.Fprintf(&sb, "Ждут ответа: %d, в работе: %d, просрочено: %d\n", st.Waiting, st.InWork, st.Overdue)
		fmt.Fprintf(&sb, "Отчётов получено: <b>%d</b>, проверено: %d\n", st.Completed, st.Reviewed)
		fmt.Fprintf(&sb, "Среднее время от принятия до отчёта: %s\n", service.FormatDuration(st.AvgTime))
		fmt.Fprintf(&sb, "\n<b>Компенсации</b>\nК выплате: %d (%s сум), выплачено: %d (%s сум), отклонено: %d\n",
			st.CompPending, fmtMoney(st.SumPending), st.CompPaid, fmtMoney(st.SumPaid), st.CompRejected)
		for _, op := range st.Operators {
			fmt.Fprintf(&sb, "\n<b>%s</b>: выдано %d, отчётов %d, просрочено %d, среднее время: %s\n",
				esc(op.Operator), op.Issued, op.Completed, op.Overdue, service.FormatDuration(op.AvgTime))
			for _, q := range op.Questions {
				label := q.Text
				if label == "" {
					label = q.Key
				}
				if q.Type == domain.QRating {
					fmt.Fprintf(&sb, "• %s: средняя оценка <b>%.1f</b> (ответов: %d)\n", esc(cut(label, 45)), q.Avg, q.N)
				} else {
					fmt.Fprintf(&sb, "• %s: «да» у <b>%.0f%%</b> (ответов: %d)\n", esc(cut(label, 45)), q.YesPc, q.N)
				}
			}
		}
	}
	return cut(sb.String(), 4000), kb(
		periodTabs("adm:sx:", period),
		row(btn("📤 Выгрузить в CSV", "adm:ex:"+period)),
		row(btn("« Назад", "adm:home")),
	)
}

func (a *App) screenExport(period string) (string, *models.InlineKeyboardMarkup) {
	_, title := a.periodFilter(period)
	text := fmt.Sprintf("<b>Выгрузка в CSV</b>, период: %s\n\n"+
		"Файлы открываются в Excel и Google Таблицах.\n"+
		"• Задания: все задания, даты этапов, напоминания, промокод, компенсация.\n"+
		"• Ответы по сценарию: одна строка на отчёт, по колонке на каждый вопрос. Фото и видео в таблице отмечены словом «приложено», сами файлы смотрите в боте.\n"+
		"• Компенсации: суммы и статусы.\n"+
		"• Журнал админов: последние 5000 действий (без учёта периода).", title)
	return text, kb(
		periodTabs("adm:ex:", period),
		row(btn("📌 Задания", "adm:exd:"+period+":t"), btn("💰 Компенсации", "adm:exd:"+period+":c")),
		row(btn("📝 Ответы по сценарию", "adm:exs:"+period), btn("📜 Журнал админов", "adm:exd:"+period+":a")),
		row(btn("« К статистике", "adm:sx:"+period)),
	)
}

func (a *App) screenExportScenarios(ctx context.Context, period string) (string, *models.InlineKeyboardMarkup) {
	list, err := a.scenarios.List(ctx, true)
	if err != nil || len(list) == 0 {
		return "Сценариев пока нет.", kb(row(btn("« Назад", "adm:ex:"+period)))
	}
	var rows [][]models.InlineKeyboardButton
	for _, s := range list {
		rows = append(rows, row(btn(cut(s.Title+" · "+s.Operator, 60), "adm:exd:"+period+":s:"+itoa(s.ID))))
	}
	rows = append(rows, row(btn("« Назад", "adm:ex:"+period)))
	return "Выберите сценарий для выгрузки ответов:", kb(rows...)
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
	case "t", "c", "s":
		if args[0] == "s" {
			if len(args) < 2 {
				return "Не выбран сценарий"
			}
			f.ScenarioID, _ = strconv.ParseInt(args[1], 10, 64)
		}
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
			data, err = a.analytics.AnswersCSV(ctx, f.ScenarioID, rows)
			name = "answers-" + itoa(f.ScenarioID) + "-" + day + ".csv"
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
