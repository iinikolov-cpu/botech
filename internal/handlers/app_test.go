package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"botech/internal/service"
	"botech/internal/storage/sqlite"
)

const testAdmin = int64(1000)

// fakeTG подменяет Telegram API и запоминает отправленные сообщения.
type fakeTG struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeTG) handler(w http.ResponseWriter, r *http.Request) {
	// Клиент бота отправляет параметры как multipart-форму или JSON.
	text := ""
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var p struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		text = p.Text
	} else {
		_ = r.ParseMultipartForm(1 << 20)
		text = r.FormValue("text")
	}
	if strings.HasSuffix(r.URL.Path, "/sendMessage") || strings.HasSuffix(r.URL.Path, "/editMessageText") {
		f.mu.Lock()
		f.sent = append(f.sent, text)
		f.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":1,"type":"private"}}}`))
}

func (f *fakeTG) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return ""
	}
	return f.sent[len(f.sent)-1]
}

func newTestBot(t *testing.T) (*bot.Bot, *fakeTG) {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	access := service.NewAccess(store, testAdmin)
	if err := access.EnsureFirstAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}

	tg := &fakeTG{}
	srv := httptest.NewServer(http.HandlerFunc(tg.handler))
	t.Cleanup(srv.Close)

	app := New(access, slog.New(slog.NewTextHandler(io.Discard, nil)), time.UTC)
	b, err := bot.New("123:TEST",
		bot.WithSkipGetMe(), bot.WithServerURL(srv.URL), bot.WithNotAsyncHandlers(),
		bot.WithMiddlewares(app.Middleware), bot.WithDefaultHandler(app.DefaultHandler),
	)
	if err != nil {
		t.Fatal(err)
	}
	app.Register(b, "testbot")
	return b, tg
}

// msg строит входящее сообщение; команды получают entity bot_command, как в настоящем Telegram.
func msg(from int64, text string) *models.Update {
	m := &models.Message{
		ID: 1, Text: text,
		From: &models.User{ID: from, FirstName: "Тест"},
		Chat: models.Chat{ID: from, Type: models.ChatTypePrivate},
	}
	if strings.HasPrefix(text, "/") {
		l := len(strings.Fields(text)[0])
		m.Entities = []models.MessageEntity{{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: l}}
	}
	return &models.Update{ID: 1, Message: m}
}

// Сквозной тест: команды доходят до своих обработчиков, а не до «справки по умолчанию».
func TestRouting(t *testing.T) {
	b, tg := newTestBot(t)
	ctx := context.Background()

	tests := []struct {
		name string
		from int64
		text string
		want string // подстрока в последнем ответе
	}{
		{"админ: /start", testAdmin, "/start", "Вы в программе"},
		{"админ: /admin", testAdmin, "/admin", "Админ-панель"},
		{"админ: /help", testAdmin, "/help", "/admin"},
		{"админ: /addadmin без аргумента", testAdmin, "/addadmin", "Формат"},
		{"админ: свободный текст", testAdmin, "привет", "Я присылаю задания"},
		{"чужой: /start", 555, "/start", "Нет доступа"},
		{"чужой: /admin", 555, "/admin", "Нет доступа"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b.ProcessUpdate(ctx, msg(tc.from, tc.text))
			if got := tg.last(); !strings.Contains(got, tc.want) {
				t.Fatalf("ответ %q не содержит %q", got, tc.want)
			}
		})
	}
}
