package handlers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"botech/internal/domain"
	"botech/internal/scenario"
)

var questionIcons = map[domain.QuestionType]string{
	domain.QText: "✏️ текст", domain.QRating: "⭐ оценка 1-5", domain.QYesNo: "☑️ да/нет",
	domain.QPhoto: "📷 фото", domain.QVideo: "🎥 видео",
}

// scenarioCallback экраны раздела «Сценарии»: adm:sc, adm:scv:<id>, adm:sca:<id>:<0|1>, adm:sct.
func (a *App) scenarioCallback(ctx context.Context, b *bot.Bot, admin *domain.User, parts []string) (string, *models.InlineKeyboardMarkup) {
	id := func() int64 {
		if len(parts) > 2 {
			v, _ := strconv.ParseInt(parts[2], 10, 64)
			return v
		}
		return 0
	}
	switch parts[1] {
	case "scv":
		return a.screenScenario(ctx, id())
	case "sca":
		archive := len(parts) > 3 && parts[3] == "1"
		if err := a.scenarios.SetArchived(ctx, admin.TgID, id(), archive); err != nil {
			a.log.Error("архивация сценария", "err", err)
		}
		return a.screenScenario(ctx, id())
	case "sct": // adm:sct (покупатель) или adm:sct:s (продавец)
		name, data, kind := "scenario_template_buyer.yaml", scenario.Template, domain.KindBuyer
		if len(parts) > 2 && parts[2] == "s" {
			name, data, kind = "scenario_template_seller.yaml", scenario.TemplateSeller, domain.KindSeller
		}
		_, err := b.SendDocument(ctx, &bot.SendDocumentParams{
			ChatID:   admin.TgID,
			Document: &models.InputFileUpload{Filename: name, Data: bytes.NewReader(data)},
			Caption:  "Шаблон сценария (" + kind.Title() + "). Заполните и отправьте файл обратно в этот чат.",
		})
		if err != nil {
			a.log.Error("отправка шаблона", "err", err)
		}
		return "", nil // экран не меняем
	}
	return a.screenScenarios(ctx)
}

func (a *App) screenScenarios(ctx context.Context) (string, *models.InlineKeyboardMarkup) {
	list, err := a.scenarios.List(ctx, true)
	if err != nil {
		a.log.Error("список сценариев", "err", err)
	}
	var sb strings.Builder
	sb.WriteString("<b>Сценарии</b>\n\n" +
		"Чтобы <b>добавить</b> или <b>обновить</b> сценарий, отправьте файл (.yaml, .yml или .json) прямо в этот чат. " +
		"Если ключ (key) уже есть, создастся новая версия, а выданные задания продолжат работать по старой.\n")
	rows := [][]models.InlineKeyboardButton{row(btn("📄 Шаблон покупателя", "adm:sct"), btn("📄 Шаблон продавца", "adm:sct:s"))}
	if len(list) == 0 {
		sb.WriteString("\nСценариев пока нет.")
	}
	for _, s := range list {
		label := fmt.Sprintf("%s · %s · %s · v%d", s.Title, s.Operator, s.Kind.Title(), s.LatestVersion)
		if s.Archived {
			label = "🗄 " + label
		}
		rows = append(rows, row(btn(cut(label, 60), "adm:scv:"+itoa(s.ID))))
	}
	rows = append(rows, row(btn("« Назад", "adm:home")))
	return sb.String(), kb(rows...)
}

func (a *App) screenScenario(ctx context.Context, id int64) (string, *models.InlineKeyboardMarkup) {
	sc, v, err := a.scenarios.Get(ctx, id)
	if err != nil {
		return "Сценарий не найден.", kb(row(btn("« Назад", "adm:sc")))
	}
	body := v.Body
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>%s</b>\nТип: %s\nОператор: %s\n", esc(body.Title), sc.Kind.Title(), esc(body.Operator))
	if body.City != "" {
		fmt.Fprintf(&sb, "Город: %s\n", esc(body.City))
	}
	if body.PVZ != "" {
		fmt.Fprintf(&sb, "ПВЗ: %s\n", esc(body.PVZ))
	}
	fmt.Fprintf(&sb, "Ключ: <code>%s</code> | версия %d\n", esc(sc.Key), v.Version)
	if sc.Archived {
		sb.WriteString("Статус: 🗄 в архиве\n")
	}
	sb.WriteString("\n<b>Инструкция</b>\n")
	for i, s := range body.Steps {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, esc(s))
	}
	sb.WriteString("\n<b>Вопросы отчёта</b>\n")
	for i, q := range body.Questions {
		req := ""
		if !q.Required {
			req = " (необязательно)"
		}
		fmt.Fprintf(&sb, "%d. %s [%s]%s\n", i+1, esc(q.Text), questionIcons[q.Type], req)
	}
	toggle := btn("🗄 В архив", "adm:sca:"+itoa(id)+":1")
	if sc.Archived {
		toggle = btn("♻ Вернуть из архива", "adm:sca:"+itoa(id)+":0")
	}
	return sb.String(), kb(row(toggle), row(btn("« К сценариям", "adm:sc")))
}

// onDocument принимает файл сценария от админа.
func (a *App) onDocument(ctx context.Context, b *bot.Bot, upd *models.Update) {
	admin := userFrom(ctx)
	doc := upd.Message.Document
	defer a.eat(ctx, b, upd.Message) // файл после обработки в чате не нужен
	back := kb(row(btn("📋 К сценариям", "adm:sc")))

	ext := strings.ToLower(filepath.Ext(doc.FileName))
	if ext == ".csv" || ext == ".txt" { // списки промокодов
		a.onPromoFile(ctx, b, admin, doc)
		return
	}
	if ext != ".yaml" && ext != ".yml" && ext != ".json" {
		a.sendPanel(ctx, b, admin.TgID, "Принимаю файлы сценариев (.yaml, .yml, .json) и списки промокодов (.csv, .txt).", back)
		return
	}
	if doc.FileSize > scenario.MaxFileSize {
		a.sendPanel(ctx, b, admin.TgID, fmt.Sprintf("Файл слишком большой (максимум %d КБ).", scenario.MaxFileSize/1024), back)
		return
	}
	data, err := a.download(ctx, b, doc.FileID, scenario.MaxFileSize)
	if err != nil {
		a.log.Error("скачивание файла сценария", "err", err)
		a.sendPanel(ctx, b, admin.TgID, "Не удалось скачать файл, попробуйте ещё раз.", back)
		return
	}

	res, problems, err := a.scenarios.Import(ctx, admin.TgID, data)
	if err != nil {
		a.log.Error("импорт сценария", "err", err)
		a.sendPanel(ctx, b, admin.TgID, "Не удалось сохранить сценарий, подробности в логах.", back)
		return
	}
	if len(problems) > 0 {
		if len(problems) > 15 {
			problems = append(problems[:15], fmt.Sprintf("...и ещё ошибок: %d", len(problems)-15))
		}
		var sb strings.Builder
		sb.WriteString("❌ Файл не принят. Исправьте и отправьте снова:\n")
		for _, p := range problems {
			sb.WriteString("• " + esc(p) + "\n")
		}
		a.sendPanel(ctx, b, admin.TgID, sb.String(), back)
		return
	}

	title := esc(res.Scenario.Title)
	var msg string
	switch {
	case res.Created:
		msg = fmt.Sprintf("✅ Сценарий «%s» создан (версия 1).", title)
	case res.Unchanged:
		msg = fmt.Sprintf("Изменений нет: файл совпадает с версией %d сценария «%s».", res.Version.Version, title)
	default:
		msg = fmt.Sprintf("✅ Сценарий «%s»: создана версия %d. Уже выданные задания остаются на прежней версии.", title, res.Version.Version)
	}
	if res.Restored {
		msg += "\nСценарий возвращён из архива."
	}
	a.log.Info("сценарий загружен", "admin", maskID(admin.TgID), "key", res.Scenario.Key, "version", res.Version.Version)
	a.sendPanel(ctx, b, admin.TgID, msg, kb(row(btn("👁 Открыть", "adm:scv:"+itoa(res.Scenario.ID))), row(btn("📋 К сценариям", "adm:sc"))))
}

// download скачивает файл, загруженный в Telegram (читает не больше limit байт).
func (a *App) download(ctx context.Context, b *bot.Bot, fileID string, limit int64) ([]byte, error) {
	f, err := b.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.FileDownloadLink(f), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// В тексте ошибки может оказаться ссылка с токеном, поэтому не пробрасываем её как есть.
		return nil, fmt.Errorf("ошибка загрузки файла")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("статус загрузки %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit+1))
}
