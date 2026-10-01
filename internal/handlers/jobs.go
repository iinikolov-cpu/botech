package handlers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"botech/internal/domain"
	"botech/internal/i18n"
	"botech/internal/service"
)

// Этот файл связывает плановые задания (напоминания, просрочка, бэкап) с Telegram:
// сервисы решают, что отправить, а здесь формируются и отправляются сообщения.

// RunReminders один проход планировщика: просрочка заданий и напоминания.
func (a *App) RunReminders(ctx context.Context, b *bot.Bot) error {
	notices, err := a.reminders.Tick(ctx)
	for _, n := range notices {
		a.deliverNotice(ctx, b, n)
	}
	return err
}

// deliverNotice отправляет одно уведомление адресату.
func (a *App) deliverNotice(ctx context.Context, b *bot.Bot, n service.Notice) {
	c := n.Card
	l := lang(c.User)
	title := esc(c.Version.Body.Title)
	id := itoa(c.Task.ID)

	switch n.Kind {
	case service.NoticeReminder:
		a.sendBuyerReminder(ctx, b, c)
		a.log.Info("напоминание отправлено", "user", maskID(c.User.TgID), "task", c.Task.ID, "phase", n.Phase, "seq", n.Seq)
	case service.NoticeExpired:
		_ = a.trySend(ctx, b, c.User.TgID, i18n.T(l, "expired_buyer", title),
			kb(row(btn(i18n.T(l, "btn_report"), "tsk:rp:"+id))))
		a.log.Info("задание просрочено", "user", maskID(c.User.TgID), "task", c.Task.ID)
	}
}

// sendBuyerReminder отправляет покупателю напоминание по текущему состоянию задания.
func (a *App) sendBuyerReminder(ctx context.Context, b *bot.Bot, c *service.TaskCard) {
	l := lang(c.User)
	title := esc(c.Version.Body.Title)
	id := itoa(c.Task.ID)
	var text string
	var markup *models.InlineKeyboardMarkup
	switch service.PhaseOf(c.Task.Status) {
	case service.PhaseAccept:
		text = i18n.T(l, "remind_accept", title)
		markup = kb(row(btn(i18n.T(l, "btn_open_task"), "tsk:v:"+id)))
	case service.PhaseRework:
		text = i18n.T(l, "remind_rework", title)
		markup = kb(row(btn(i18n.T(l, "btn_fix_report"), "tsk:rp:"+id)))
	default:
		due := ""
		if !c.Task.DueAt.IsZero() {
			due = i18n.T(l, "remind_due_line", a.fmtTime(c.Task.DueAt))
		}
		text = i18n.T(l, "remind_report", title, due)
		markup = kb(row(btn(i18n.T(l, "btn_report"), "tsk:rp:"+id)))
	}
	if err := a.trySend(ctx, b, c.User.TgID, text, markup); err != nil {
		a.log.Warn("напоминание не доставлено", "user", maskID(c.User.TgID), "task", c.Task.ID, "err", err)
	}
}

// humanDuration «3 дн. 2 ч» или «5 ч».
func humanDuration(d time.Duration) string {
	h := int(d.Hours())
	if h < 1 {
		return "менее часа"
	}
	if h >= 24 {
		if r := h % 24; r > 0 {
			return fmt.Sprintf("%d дн. %d ч", h/24, r)
		}
		return fmt.Sprintf("%d дн.", h/24)
	}
	return fmt.Sprintf("%d ч", h)
}

// pingCallback ручные напоминания: adm:tp:<taskID> (по заданию), adm:up:<userID> (по всем заданиям покупателя).
func (a *App) pingCallback(ctx context.Context, b *bot.Bot, admin *domain.User, parts []string) (string, *models.InlineKeyboardMarkup, string) {
	id, _ := strconv.ParseInt(parts[2], 10, 64)
	if parts[1] == "tp" {
		toast := "Напоминание отправлено"
		card, err := a.reminders.Ping(ctx, admin.TgID, id)
		if err != nil {
			toast = errText(err)
		} else {
			a.sendBuyerReminder(ctx, b, card)
			a.log.Info("ручное напоминание", "admin", maskID(admin.TgID), "task", id)
		}
		t, m := a.screenTask(ctx, id)
		return t, m, toast
	}
	cards, throttled, err := a.reminders.PingUser(ctx, admin.TgID, id)
	toast := fmt.Sprintf("Отправлено напоминаний: %d", len(cards))
	switch {
	case err != nil:
		a.log.Error("напоминание покупателю", "err", err)
		toast = "Ошибка, подробности в логах"
	case len(cards) == 0 && throttled == 0:
		toast = "У покупателя нет незавершённых заданий"
	case len(cards) == 0:
		toast = "Недавно уже напоминали, повторите позже"
	case throttled > 0:
		toast += fmt.Sprintf(" (ещё %d недавно уже получали напоминание)", throttled)
	}
	for _, c := range cards {
		a.sendBuyerReminder(ctx, b, c)
	}
	t, m := a.screenUser(ctx, id)
	return t, m, toast
}

// ---------- Настройки ----------

const settingDialog = "setting"

// settingInfo как называется настройка и что вводить. Интервалы напоминаний задаются в конфиге (.env), не здесь.
var settingInfo = map[string]struct{ title, hint string }{
	service.KeyBackupTime: {"Время ежедневного бэкапа", "Во сколько присылать копию базы (по времени бота). Формат: 03:00 или off."},
}

var settingCodes = map[string]string{"backup": service.KeyBackupTime}

// settingsCallback экран настроек: adm:st, adm:ste:<код>, adm:stc, adm:stb.
func (a *App) settingsCallback(ctx context.Context, b *bot.Bot, admin *domain.User, parts []string) (string, *models.InlineKeyboardMarkup, string) {
	switch parts[1] {
	case "ste":
		key := settingCodes[parts[2]]
		info, ok := settingInfo[key]
		if !ok {
			break
		}
		if err := a.dialog.Set(ctx, admin.TgID, settingDialog, struct {
			Key string `json:"key"`
		}{key}); err != nil {
			a.log.Error("состояние настройки", "err", err)
		}
		return fmt.Sprintf("✏ <b>%s</b>\n\n%s\n\nОтправьте новое значение сообщением.", info.title, info.hint),
			kb(row(btn("Отмена", "adm:stc"))), ""
	case "stc":
		_ = a.dialog.Clear(ctx, admin.TgID)
	case "stb":
		a.backupNow(ctx, b, admin)
		return "", nil, "Копия создаётся и отправляется"
	}
	t, m := a.screenSettings(ctx)
	return t, m, ""
}

func (a *App) screenSettings(ctx context.Context) (string, *models.InlineKeyboardMarkup) {
	bk, _ := a.settings.Backup(ctx)
	last, _ := a.settings.Value(ctx, service.KeyLastBackup)

	var sb strings.Builder
	sb.WriteString("<b>Настройки</b>\n\n")
	if bk.Enabled {
		fmt.Fprintf(&sb, "Бэкап базы: ежедневно в %02d:%02d (%s), последний: %s\n", bk.Hour, bk.Minute, a.loc, orDash(last))
	} else {
		sb.WriteString("Бэкап базы: отключён\n")
	}
	sb.WriteString("\nИнтервалы напоминаний и тихие часы задаются в файле .env на сервере.")
	return sb.String(), kb(
		row(btn("✏ Время бэкапа", "adm:ste:backup"), btn("💾 Бэкап сейчас", "adm:stb")),
		row(btn("« Назад", "adm:home")),
	)
}

func orDash(s string) string {
	if s == "" {
		return "ещё не было"
	}
	return s
}

// onSettingValue админ прислал новое значение настройки.
func (a *App) onSettingValue(ctx context.Context, b *bot.Bot, admin *domain.User, text string) {
	var st struct {
		Key string `json:"key"`
	}
	if name, _ := a.dialog.Get(ctx, admin.TgID, &st); name != settingDialog {
		return
	}
	val, err := a.settings.Set(ctx, admin.TgID, st.Key, text)
	if err != nil {
		a.sendKeyed(ctx, b, admin.TgID, "err", "❌ "+esc(errText(err))+"\nПопробуйте ещё раз или нажмите «Отмена» выше.", nil)
		return
	}
	_ = a.dialog.Clear(ctx, admin.TgID)
	a.log.Info("настройка изменена", "admin", maskID(admin.TgID), "key", st.Key)
	t, m := a.screenSettings(ctx)
	a.sendPanel(ctx, b, admin.TgID, "✅ Сохранено: <code>"+esc(val)+"</code>\n\n"+t, m)
}

// ---------- Бэкап ----------

// RunBackup ежедневная копия базы: создаёт и отправляет файл, когда наступило время и сегодня ещё не делали.
func (a *App) RunBackup(ctx context.Context, b *bot.Bot) error {
	bk, err := a.settings.Backup(ctx)
	if err != nil || !bk.Enabled {
		return err
	}
	now := time.Now().In(a.loc)
	scheduled := time.Date(now.Year(), now.Month(), now.Day(), bk.Hour, bk.Minute, 0, 0, a.loc)
	today := now.Format("2006-01-02")
	if now.Before(scheduled) {
		return nil
	}
	if last, _ := a.settings.Value(ctx, service.KeyLastBackup); last == today {
		return nil
	}
	file, err := a.backups.Create(ctx)
	if err != nil {
		// Об ошибке предупреждаем один раз в сутки, а не каждые несколько минут.
		if failed, _ := a.settings.Value(ctx, service.KeyBackupFail); failed != today {
			a.send(ctx, b, a.backupChat, "⚠ Не удалось сделать ежедневную копию базы. Подробности в логах бота.", nil)
			_ = a.settings.SetInternal(ctx, service.KeyBackupFail, today)
		}
		return err
	}
	a.deliverBackup(ctx, b, a.backupChat, file, "Ежедневная копия базы")
	a.log.Info("бэкап создан", "size", file.Size)
	return a.settings.SetInternal(ctx, service.KeyLastBackup, today)
}

// backupNow копия по кнопке «Бэкап сейчас»: создаётся и отправляется нажавшему админу.
func (a *App) backupNow(ctx context.Context, b *bot.Bot, admin *domain.User) {
	file, err := a.backups.Create(ctx)
	if err != nil {
		a.log.Error("бэкап по кнопке", "err", err)
		a.send(ctx, b, admin.TgID, "⚠ Не удалось сделать копию базы. Подробности в логах бота.", nil)
		return
	}
	a.log.Info("бэкап создан по кнопке", "admin", maskID(admin.TgID), "size", file.Size)
	a.deliverBackup(ctx, b, admin.TgID, file, "Копия базы по запросу")
}

// deliverBackup отправляет файл копии в Telegram. Слишком большой файл остаётся на сервере.
func (a *App) deliverBackup(ctx context.Context, b *bot.Bot, chatID int64, f *service.BackupFile, title string) {
	if f.Size > service.MaxTelegramFile {
		a.send(ctx, b, chatID, fmt.Sprintf("⚠ Копия базы создана, но слишком велика для Telegram (%d МБ). Она лежит на сервере: <code>%s</code>", f.Size>>20, esc(f.Path)), nil)
		return
	}
	file, err := os.Open(f.Path)
	if err != nil {
		a.log.Error("открытие копии", "err", err)
		return
	}
	defer file.Close()
	_, err = b.SendDocument(ctx, &bot.SendDocumentParams{
		ChatID:   chatID,
		Document: &models.InputFileUpload{Filename: filepath.Base(f.Path), Data: file},
		Caption: fmt.Sprintf("💾 %s, %s, %d КБ.\nВ копии персональные данные покупателей: храните её в надёжном месте. Восстановление описано в инструкции.",
			title, time.Now().In(a.loc).Format("02.01.2006 15:04"), (f.Size+1023)/1024),
	})
	if err != nil {
		a.log.Error("отправка копии", "err", err)
	}
}
