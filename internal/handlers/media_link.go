package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"

	"botech/internal/domain"
)

// startPayload параметр после /start ("" если его нет).
func startPayload(text string) string {
	f := strings.Fields(text)
	if len(f) >= 2 && strings.HasPrefix(f[0], "/start") {
		return f[1]
	}
	return ""
}

// parseMediaPayload разбирает "m<номер отчёта>x<ключ вопроса>".
func parseMediaPayload(p string) (reportID int64, key string, ok bool) {
	if !strings.HasPrefix(p, "m") {
		return 0, "", false
	}
	num, key, found := strings.Cut(p[1:], "x")
	if !found || key == "" {
		return 0, "", false
	}
	id, err := strconv.ParseInt(num, 10, 64)
	if err != nil || id < 1 {
		return 0, "", false
	}
	return id, key, true
}

// sendReportMedia присылает админу фото или видео из ответа отчёта (по ссылке из выгрузки).
func (a *App) sendReportMedia(ctx context.Context, b *bot.Bot, admin *domain.User, reportID int64, key string) {
	rep, err := a.reports.ReportByID(ctx, reportID)
	if err != nil {
		a.sendKeyed(ctx, b, admin.TgID, "err", "Отчёт не найден.", nil)
		return
	}
	var ans *domain.Answer
	for i := range rep.Answers {
		if rep.Answers[i].Key == key && !rep.Answers[i].Skipped && rep.Answers[i].FileID != "" {
			ans = &rep.Answers[i]
		}
	}
	if ans == nil {
		a.sendKeyed(ctx, b, admin.TgID, "err", "В этом отчёте нет такого файла.", nil)
		return
	}
	caption := fmt.Sprintf("Задание #%d, отчёт версии %d", rep.TaskID, rep.Revision)
	if card, err := a.tasks.Card(ctx, rep.TaskID); err == nil {
		for _, q := range card.Version.Body.Questions {
			if q.Key == key {
				caption += ": " + cut(q.Text, 200)
			}
		}
	}
	a.log.Info("медиа по ссылке из выгрузки", "admin", maskID(admin.TgID), "report", reportID)
	a.sendMedia(ctx, b, admin.TgID, []mediaItem{{Video: ans.Type == domain.QVideo, FileID: ans.FileID, Caption: caption}})
}
