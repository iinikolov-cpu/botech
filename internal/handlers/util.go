package handlers

import (
	"context"
	"fmt"
	"html"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"botech/internal/domain"
)

// Callback-данные ограничены 64 байтами, поэтому формат короткий: "adm:<раздел>:<аргументы>".

type ctxKey struct{}

// withUser кладёт пользователя в контекст (его проверил middleware доступа).
func withUser(ctx context.Context, u *domain.User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

func userFrom(ctx context.Context) *domain.User {
	u, _ := ctx.Value(ctxKey{}).(*domain.User)
	return u
}

// esc экранирует текст для parse_mode=HTML.
func esc(s string) string { return html.EscapeString(s) }

// maskID скрывает Telegram ID для логов: остаются только последние 4 цифры.
func maskID(id int64) string {
	s := fmt.Sprint(id)
	if len(s) <= 4 {
		return "***"
	}
	return "***" + s[len(s)-4:]
}

// userLabel человекочитаемая подпись пользователя со ссылкой на профиль.
func userLabel(u *domain.User) string {
	name := strings.TrimSpace(u.FirstName)
	if name == "" {
		name = "без имени"
	}
	s := fmt.Sprintf(`<a href="tg://user?id=%d">%s</a>`, u.TgID, esc(name))
	if u.Username != "" {
		s += " @" + esc(u.Username)
	}
	return s
}

func btn(text, data string) models.InlineKeyboardButton {
	return models.InlineKeyboardButton{Text: text, CallbackData: data}
}

func kb(rows ...[]models.InlineKeyboardButton) *models.InlineKeyboardMarkup {
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func row(b ...models.InlineKeyboardButton) []models.InlineKeyboardButton { return b }

// send отправляет HTML-сообщение; ошибки только логируются.
func (a *App) send(ctx context.Context, b *bot.Bot, chatID int64, text string, markup *models.InlineKeyboardMarkup) {
	if err := a.trySend(ctx, b, chatID, text, markup); err != nil {
		a.log.Warn("не удалось отправить сообщение", "user", maskID(chatID), "err", err)
	}
}

// trySend отправляет HTML-сообщение и возвращает ошибку (нужно там, где важен факт доставки).
func (a *App) trySend(ctx context.Context, b *bot.Bot, chatID int64, text string, markup *models.InlineKeyboardMarkup) error {
	p := &bot.SendMessageParams{
		ChatID: chatID, Text: text, ParseMode: models.ParseModeHTML,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
	}
	if markup != nil {
		p.ReplyMarkup = markup
	}
	_, err := b.SendMessage(ctx, p)
	return err
}

// edit заменяет текст сообщения с кнопкой (для навигации по меню).
func (a *App) edit(ctx context.Context, b *bot.Bot, cb *models.CallbackQuery, text string, markup *models.InlineKeyboardMarkup) {
	msg := cb.Message.Message
	if msg == nil {
		a.send(ctx, b, cb.From.ID, text, markup)
		return
	}
	var err error
	if msg.Photo != nil || msg.Video != nil || msg.Document != nil {
		// У сообщений с медиа меняется подпись, а не текст.
		p := &bot.EditMessageCaptionParams{ChatID: msg.Chat.ID, MessageID: msg.ID, Caption: cut(text, 1000), ParseMode: models.ParseModeHTML}
		if markup != nil {
			p.ReplyMarkup = markup
		}
		_, err = b.EditMessageCaption(ctx, p)
	} else {
		p := &bot.EditMessageTextParams{
			ChatID: msg.Chat.ID, MessageID: msg.ID, Text: text, ParseMode: models.ParseModeHTML,
			LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
		}
		if markup != nil {
			p.ReplyMarkup = markup
		}
		_, err = b.EditMessageText(ctx, p)
	}
	if err != nil && !strings.Contains(err.Error(), "not modified") {
		a.log.Warn("не удалось изменить сообщение", "user", maskID(cb.From.ID), "err", err)
	}
}

// answerCB закрывает «часики» на кнопке; alert показывает всплывающее окно.
func (a *App) answerCB(ctx context.Context, b *bot.Bot, id, text string, alert bool) {
	_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: id, Text: text, ShowAlert: alert})
}

// limiter простой ограничитель «не больше n событий за окно» на ключ.
type limiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	events map[int64][]time.Time
	now    func() time.Time
}

func newLimiter(n int, window time.Duration) *limiter {
	return &limiter{n: n, window: window, events: map[int64][]time.Time{}, now: time.Now}
}

// Allow фиксирует событие и возвращает false, если лимит превышен.
func (l *limiter) Allow(key int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cut := now.Add(-l.window)
	kept := l.events[key][:0]
	for _, t := range l.events[key] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.n {
		l.events[key] = kept
		return false
	}
	l.events[key] = append(kept, now)
	return true
}

// Blocked true, если лимит уже исчерпан (без записи нового события).
func (l *limiter) Blocked(key int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := l.now().Add(-l.window)
	c := 0
	for _, t := range l.events[key] {
		if t.After(cut) {
			c++
		}
	}
	return c >= l.n
}

// mediaItem фото или видео из отчёта для показа админу.
type mediaItem struct {
	Video   bool
	FileID  string
	Caption string
}

// sendMedia отправляет фото и видео компактно: альбомами по 10 штук (одиночный файл обычным
// сообщением). Вызывать нужно ДО сообщения с кнопками, чтобы кнопки оказались в самом низу.
func (a *App) sendMedia(ctx context.Context, b *bot.Bot, chatID int64, items []mediaItem) {
	single := func(m mediaItem) {
		var err error
		if m.Video {
			_, err = b.SendVideo(ctx, &bot.SendVideoParams{ChatID: chatID, Video: &models.InputFileString{Data: m.FileID}, Caption: m.Caption})
		} else {
			_, err = b.SendPhoto(ctx, &bot.SendPhotoParams{ChatID: chatID, Photo: &models.InputFileString{Data: m.FileID}, Caption: m.Caption})
		}
		if err != nil {
			a.log.Warn("не удалось отправить медиа", "err", err)
		}
	}
	for len(items) > 0 {
		n := len(items)
		if n > 10 {
			n = 10
		}
		chunk := items[:n]
		items = items[n:]
		if len(chunk) == 1 {
			single(chunk[0])
			continue
		}
		group := make([]models.InputMedia, 0, len(chunk))
		for _, m := range chunk {
			if m.Video {
				group = append(group, &models.InputMediaVideo{Media: m.FileID, Caption: m.Caption})
			} else {
				group = append(group, &models.InputMediaPhoto{Media: m.FileID, Caption: m.Caption})
			}
		}
		if _, err := b.SendMediaGroup(ctx, &bot.SendMediaGroupParams{ChatID: chatID, Media: group}); err != nil {
			a.log.Warn("альбом не отправился, отправляю по одному", "err", err)
			for _, m := range chunk {
				single(m)
			}
		}
	}
}
