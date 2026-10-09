package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"botech/internal/domain"
	"botech/internal/service"
	"botech/internal/storage/sqlite"
)

const testAdmin = int64(1000)

// sentMsg сообщение, отправленное ботом через (поддельный) Telegram API.
type sentMsg struct {
	Method   string
	Chat     int64
	Text     string
	Markup   string // reply_markup как JSON-строка (содержит callback-данные кнопок)
	FileName string // для sendDocument: имя загруженного файла
	FileSize int    // и его размер в байтах
}

// fakeTG подменяет Telegram API: запоминает отправленное и отдаёт файлы.
type fakeTG struct {
	mu      sync.Mutex
	sent    []sentMsg
	answers []string          // тексты answerCallbackQuery
	nextID  int               // счётчик message_id
	deleted []int             // id удалённых сообщений
	files   map[string]string // file_id -> содержимое
}

func (f *fakeTG) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if strings.Contains(r.URL.Path, "/file/") { // скачивание файла
		id := strings.TrimPrefix(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "")
		f.mu.Lock()
		content := f.files[id]
		f.mu.Unlock()
		_, _ = w.Write([]byte(content))
		return
	}
	// Клиент бота отправляет параметры как multipart-форму или JSON.
	form := map[string]string{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var raw map[string]any
		_ = json.NewDecoder(r.Body).Decode(&raw)
		for k, v := range raw {
			if str, ok := v.(string); ok {
				form[k] = str
			} else {
				b, _ := json.Marshal(v)
				form[k] = string(b)
			}
		}
	} else {
		_ = r.ParseMultipartForm(1 << 20)
		for k, v := range r.MultipartForm.Value {
			form[k] = v[0]
		}
	}
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	f.mu.Lock()
	switch method {
	case "sendMessage", "editMessageText":
		chat, _ := strconv.ParseInt(form["chat_id"], 10, 64)
		f.sent = append(f.sent, sentMsg{Method: method, Chat: chat, Text: form["text"], Markup: form["reply_markup"]})
	case "sendPhoto", "sendVideo":
		chat, _ := strconv.ParseInt(form["chat_id"], 10, 64)
		f.sent = append(f.sent, sentMsg{Method: method, Chat: chat, Text: form["caption"], Markup: form["reply_markup"]})
	case "sendDocument":
		chat, _ := strconv.ParseInt(form["chat_id"], 10, 64)
		m := sentMsg{Method: method, Chat: chat, Text: form["caption"]}
		if r.MultipartForm != nil {
			for _, fhs := range r.MultipartForm.File {
				if len(fhs) > 0 {
					m.FileName, m.FileSize = fhs[0].Filename, int(fhs[0].Size)
				}
			}
		}
		f.sent = append(f.sent, m)
	case "sendMediaGroup":
		chat, _ := strconv.ParseInt(form["chat_id"], 10, 64)
		f.sent = append(f.sent, sentMsg{Method: method, Chat: chat, Text: form["media"]})
		a, b := f.newID(), f.newID()
		f.mu.Unlock()
		// Telegram отвечает массивом сообщений альбома.
		fmt.Fprintf(w, `{"ok":true,"result":[{"message_id":%d,"date":1,"chat":{"id":1,"type":"private"}},{"message_id":%d,"date":1,"chat":{"id":1,"type":"private"}}]}`, a, b)
		return
	case "deleteMessage":
		id, _ := strconv.Atoi(form["message_id"])
		f.deleted = append(f.deleted, id)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		return
	case "editMessageCaption":
		chat, _ := strconv.ParseInt(form["chat_id"], 10, 64)
		f.sent = append(f.sent, sentMsg{Method: method, Chat: chat, Text: form["caption"], Markup: form["reply_markup"]})
	case "answerCallbackQuery":
		f.answers = append(f.answers, form["text"])
	case "getFile":
		id := form["file_id"]
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"` + id + `","file_path":"` + id + `"}}`))
		return
	}
	id := f.newID()
	f.mu.Unlock()
	fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"date":1,"chat":{"id":1,"type":"private"}}}`, id)
}

// newID выдаёт уникальный message_id (вызывать под f.mu).
func (f *fakeTG) newID() int {
	f.nextID++
	return f.nextID
}

// deletedCount сколько сообщений удалено.
func (f *fakeTG) deletedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deleted)
}

func (f *fakeTG) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return ""
	}
	return f.sent[len(f.sent)-1].Text
}

// lastTo последнее сообщение, отправленное конкретному чату.
func (f *fakeTG) lastTo(chat int64) (sentMsg, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if f.sent[i].Chat == chat {
			return f.sent[i], true
		}
	}
	return sentMsg{}, false
}

// anyTo true, если чату отправляли сообщение с подстрокой (не только последнее).
func (f *fakeTG) anyTo(chat int64, substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.sent {
		if m.Chat == chat && strings.Contains(m.Text, substr) {
			return true
		}
	}
	return false
}

// lastMsg последнее отправленное сообщение целиком.
func (f *fakeTG) lastMsg() sentMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return sentMsg{}
	}
	return f.sent[len(f.sent)-1]
}

func (f *fakeTG) lastAnswer() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.answers) == 0 {
		return ""
	}
	return f.answers[len(f.answers)-1]
}

type testEnv struct {
	b     *bot.Bot
	tg    *fakeTG
	store *sqlite.Store
	app   *App
}

func newTestBot(t *testing.T) (*bot.Bot, *fakeTG) {
	e := newTestEnv(t)
	return e.b, e.tg
}

func newTestEnv(t *testing.T) *testEnv { return newTestEnvUses(t, 1) }

// newTestEnvUses то же, но с заданным лимитом использований промокода.
func newTestEnvUses(t *testing.T, promoUses int) *testEnv {
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

	tg := &fakeTG{files: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(tg.handler))
	t.Cleanup(srv.Close)

	tasks := service.NewTasks(store, promoUses)
	settings := service.NewSettings(store)
	app := New(Services{
		Access: access, Scenarios: service.NewScenarios(store), Tasks: tasks, Dialog: service.NewDialog(store),
		Promos: service.NewPromos(store, promoUses), Reports: service.NewReports(store, tasks),
		Reminders: service.NewReminders(store, tasks, service.RemindConfig{
			Accept: service.MinutesToDurations([]int{1440, 2880}), Report: service.MinutesToDurations([]int{1440, 2880}),
			QuietOn: true, QuietFrom: 22, QuietTo: 9, Stale: 24 * time.Hour,
		}, time.UTC), Settings: settings,
		Analytics:    service.NewAnalytics(store, time.UTC),
		Backups:      service.NewBackups(store, filepath.Join(t.TempDir(), "backups"), 3),
		BackupChatID: testAdmin,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.UTC)
	b, err := bot.New("123:TEST",
		bot.WithSkipGetMe(), bot.WithServerURL(srv.URL), bot.WithNotAsyncHandlers(),
		bot.WithMiddlewares(app.Middleware), bot.WithDefaultHandler(app.DefaultHandler),
	)
	if err != nil {
		t.Fatal(err)
	}
	app.Register(b, "testbot")
	return &testEnv{b: b, tg: tg, store: store, app: app}
}

// addBuyer создаёт активного покупателя.
func (e *testEnv) addBuyer(t *testing.T, id int64, name string) {
	t.Helper()
	now := time.Now().UTC()
	if err := e.store.Repos().Users.Create(context.Background(), &domain.User{
		TgID: id, Role: domain.RoleBuyer, Status: domain.StatusActive, Lang: "ru", FirstName: name, Kinds: domain.AllKinds, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

// click имитирует нажатие inline-кнопки.
func (e *testEnv) click(from int64, data string) {
	e.b.ProcessUpdate(context.Background(), &models.Update{ID: 1, CallbackQuery: &models.CallbackQuery{
		ID: "cb", From: models.User{ID: from, FirstName: "Тест"}, Data: data,
		Message: models.MaybeInaccessibleMessage{
			Type:    models.MaybeInaccessibleMessageTypeMessage,
			Message: &models.Message{ID: 10, Chat: models.Chat{ID: from, Type: models.ChatTypePrivate}},
		},
	}})
}

// upload имитирует отправку файла-документа.
func (e *testEnv) upload(from int64, name, content string) {
	e.tg.mu.Lock()
	e.tg.files[name] = content
	e.tg.mu.Unlock()
	e.b.ProcessUpdate(context.Background(), &models.Update{ID: 1, Message: &models.Message{
		ID: 2, From: &models.User{ID: from, FirstName: "Тест"}, Chat: models.Chat{ID: from, Type: models.ChatTypePrivate},
		Document: &models.Document{FileID: name, FileName: name, FileSize: int64(len(content))},
	}})
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

const flowScenario = `key: flow-test
title: Проверка ПВЗ
operator: BTS
city: Ташкент
steps: [Сделать раз, Сделать два]
questions:
  - {key: q_one, text: Как обслужили, type: rating}
`

// Сквозной сценарий: загрузка сценария, назначение, принятие покупателем, защита от чужих.
func TestScenarioAndTaskFlow(t *testing.T) {
	e := newTestEnv(t)
	e.addBuyer(t, 2000, "Алия")
	e.addBuyer(t, 2001, "Борис")

	// 1. Админ загружает сценарий файлом.
	e.upload(testAdmin, "s.yaml", flowScenario)
	if got := e.tg.last(); !strings.Contains(got, "создан") {
		t.Fatalf("загрузка сценария: %q", got)
	}
	// Некорректный файл даёт понятные ошибки.
	e.upload(testAdmin, "bad.yaml", "key: x")
	if got := e.tg.last(); !strings.Contains(got, "Файл не принят") {
		t.Fatalf("плохой файл: %q", got)
	}
	// Файл от покупателя игнорируется.
	before := len(e.tg.sent)
	e.upload(2000, "s2.yaml", flowScenario)
	if len(e.tg.sent) != before {
		t.Fatal("файл от не-админа должен игнорироваться")
	}

	// 2. Мастер назначения.
	for _, data := range []string{"adm:as:0", "adm:as:s:1", "adm:as:d:3", "adm:as:t:2000:0", "adm:as:go"} {
		e.click(testAdmin, data)
	}
	if got := e.tg.last(); !strings.Contains(got, "Отправлено: 1") {
		t.Fatalf("итог назначения: %q", got)
	}
	msg, ok := e.tg.lastTo(2000)
	if !ok || !strings.Contains(msg.Text, "Вам новое задание") || !strings.Contains(msg.Text, "Сделать два") {
		t.Fatalf("покупатель не получил инструкцию: %+v", msg)
	}
	if !strings.Contains(msg.Markup, "tsk:ac:1") || !strings.Contains(msg.Markup, "tsk:dc:1") {
		t.Fatalf("нет кнопок принять/отказаться: %s", msg.Markup)
	}

	// 3. Защита: чужой покупатель и посторонний не видят задание.
	e.click(2001, "tsk:v:1")
	if got := e.tg.lastAnswer(); got != "Задание не найдено." {
		t.Fatalf("чужой покупатель: %q", got)
	}
	e.click(2001, "tsk:ac:1")
	if got := e.tg.lastAnswer(); got != "Задание не найдено." {
		t.Fatalf("чужой не должен принять: %q", got)
	}
	e.click(555, "tsk:v:1")
	if got := e.tg.lastAnswer(); got != "Нет доступа." {
		t.Fatalf("посторонний: %q", got)
	}
	e.click(2000, "adm:home")
	if got := e.tg.lastAnswer(); got != "Нет доступа." {
		t.Fatalf("покупатель в админке: %q", got)
	}

	// 4. Покупатель принимает; админ получает уведомление.
	e.click(2000, "tsk:ac:1")
	if got := e.tg.last(); !strings.Contains(got, "принято") || !strings.Contains(got, "Выполнить до") {
		t.Fatalf("карточка после принятия: %q", got)
	}
	if !e.tg.anyTo(testAdmin, "принял задание") {
		t.Fatal("админ не уведомлён о принятии")
	}
	// Повторное нажатие и отказ после принятия ничего не ломают.
	e.click(2000, "tsk:ac:1")
	if got := e.tg.lastAnswer(); got != "Это задание уже обработано." {
		t.Fatalf("повторное принятие: %q", got)
	}
	e.click(2000, "tsk:dy:1")
	if got := e.tg.last(); !strings.Contains(got, "принято") {
		t.Fatalf("отказ после принятия не должен менять статус: %q", got)
	}

	// 5. Админ видит задание в списке и карточке с историей.
	e.click(testAdmin, "adm:tk:p:0")
	if got := e.tg.last(); !strings.Contains(got, "найдено: 1") {
		t.Fatalf("список «в работе»: %q", got)
	}
	e.click(testAdmin, "adm:tc:1")
	if got := e.tg.last(); !strings.Contains(got, "отправлено → принято") {
		t.Fatalf("история в карточке: %q", got)
	}
}

// Отказ требует подтверждения; после него сценарий можно выдать снова.
func TestDeclineNeedsConfirmation(t *testing.T) {
	e := newTestEnv(t)
	e.addBuyer(t, 2000, "Алия")
	e.upload(testAdmin, "s.yaml", flowScenario)
	for _, data := range []string{"adm:as:0", "adm:as:s:1", "adm:as:d:2", "adm:as:t:2000:0", "adm:as:go"} {
		e.click(testAdmin, data)
	}
	e.click(2000, "tsk:dc:1")
	if got := e.tg.last(); !strings.Contains(got, "Точно отказаться") {
		t.Fatalf("нет подтверждения: %q", got)
	}
	e.click(2000, "tsk:dy:1")
	if got := e.tg.last(); !strings.Contains(got, "отказ") {
		t.Fatalf("после подтверждения: %q", got)
	}
}

// say отправляет текстовое сообщение от имени пользователя.
func (e *testEnv) say(from int64, text string) {
	e.b.ProcessUpdate(context.Background(), msg(from, text))
}

// sendPhoto имитирует отправку фото (Telegram присылает несколько размеров).
func (e *testEnv) sendPhoto(from int64, id string) {
	e.b.ProcessUpdate(context.Background(), &models.Update{ID: 1, Message: &models.Message{
		ID: 3, From: &models.User{ID: from, FirstName: "Тест"}, Chat: models.Chat{ID: from, Type: models.ChatTypePrivate},
		Photo: []models.PhotoSize{{FileID: id + "-small", FileUniqueID: id + "-u1"}, {FileID: id, FileUniqueID: id + "-u"}},
	}})
}

var dataRe = regexp.MustCompile(`"callback_data":"([^"]+)"`)

// press нажимает первую кнопку последнего сообщения чату, чьи данные начинаются с prefix и оканчиваются на suffix.
func (e *testEnv) press(t *testing.T, chat int64, prefix, suffix string) {
	t.Helper()
	m, ok := e.tg.lastTo(chat)
	if !ok {
		t.Fatalf("нет сообщений для %d", chat)
	}
	for _, sub := range dataRe.FindAllStringSubmatch(m.Markup, -1) {
		if strings.HasPrefix(sub[1], prefix) && strings.HasSuffix(sub[1], suffix) {
			e.click(chat, sub[1])
			return
		}
	}
	t.Fatalf("в последнем сообщении нет кнопки %s*%s: %q %s", prefix, suffix, m.Text, m.Markup)
}

const reportFlowScenario = `key: report-flow
title: Полный отчёт
operator: BTS
steps: [Шаг]
questions:
  - {key: rate, text: Оценка сервиса, type: rating}
  - {key: yn, text: Проверили документ, type: yesno}
  - {key: photo, text: Фото посылки, type: photo}
  - {key: note, text: Комментарий, type: text, required: false}
`

// Полный путь этапа 3: промокоды, выдача при принятии, предупреждение о нехватке,
// мастер отчёта с компенсацией, просмотр и выплата админом.
func TestPromoReportCompensationFlow(t *testing.T) {
	e := newTestEnv(t)
	e.addBuyer(t, 2000, "Алия")
	e.addBuyer(t, 2001, "Борис")
	e.upload(testAdmin, "s.yaml", reportFlowScenario)

	// Загрузка промокодов текстом и файлом.
	e.click(testAdmin, "adm:pra")
	e.say(testAdmin, "AAA111\nBBB222\nCCC333")
	if got := e.tg.last(); !strings.Contains(got, "Добавлено кодов: <b>3</b>") {
		t.Fatalf("загрузка текстом: %q", got)
	}
	e.upload(testAdmin, "codes.csv", "code,note\nDDD444,x\nAAA111,повтор")
	if got := e.tg.last(); !strings.Contains(got, "Добавлено кодов: <b>1</b>") || !strings.Contains(got, "повторов: 1") {
		t.Fatalf("загрузка файлом: %q", got)
	}

	assign := func(user int64) {
		for _, d := range []string{"adm:as:0", "adm:as:s:1", "adm:as:d:3", fmt.Sprintf("adm:as:t:%d:0", user), "adm:as:go"} {
			e.click(testAdmin, d)
		}
	}
	// Первое принятие: выдан AAA111, остаток 3, предупреждения нет.
	assign(2000)
	e.click(2000, "tsk:ac:1")
	if got := e.tg.last(); !strings.Contains(got, "AAA111") {
		t.Fatalf("промокод не показан покупателю: %q", got)
	}
	// Второе принятие: выдан BBB222, предупреждений нет (коды ещё есть).
	assign(2001)
	e.click(2001, "tsk:ac:2")
	if got := e.tg.last(); !strings.Contains(got, "BBB222") {
		t.Fatalf("второй код: %q", got)
	}
	if e.tg.anyTo(testAdmin, "⚠") {
		t.Fatal("предупреждений быть не должно, пока есть свободные коды")
	}
	// Таблица кодов: два занятых, два свободных.
	e.click(testAdmin, "adm:prl:0")
	if got := e.tg.last(); !strings.Contains(got, "AAA111") || !strings.Contains(got, "#1") || !strings.Contains(got, "CCC333") {
		t.Fatalf("таблица кодов: %q", got)
	}

	// Мастер отчёта.
	e.click(2000, "tsk:rp:1")
	if m, _ := e.tg.lastTo(2000); !strings.Contains(m.Text, "Вопрос 1 из 4") {
		t.Fatalf("первый вопрос: %q", m.Text)
	}
	staleData := ""
	if m, _ := e.tg.lastTo(2000); true {
		staleData = dataRe.FindStringSubmatch(m.Markup)[1] // кнопка первого шага
	}
	e.press(t, 2000, "rpt:r:", ":5")
	e.click(2000, staleData) // устаревшая кнопка
	if got := e.tg.lastAnswer(); got != "Эта кнопка устарела." {
		t.Fatalf("устаревшая кнопка: %q", got)
	}
	e.press(t, 2000, "rpt:y:", ":yes")
	e.say(2000, "просто текст") // ждём фото
	if got := e.tg.last(); !strings.Contains(got, "Жду фото") {
		t.Fatalf("ожидание фото: %q", got)
	}
	e.sendPhoto(2000, "PHOTO1")
	if m, _ := e.tg.lastTo(2000); !strings.Contains(m.Text, "Вопрос 4 из 4") || !strings.Contains(m.Text, "необязательный") {
		t.Fatalf("четвёртый вопрос: %q", m.Text)
	}
	e.press(t, 2000, "rpt:s:", "") // пропустить необязательный
	if m, _ := e.tg.lastTo(2000); !strings.Contains(m.Text, "Компенсация") {
		t.Fatalf("вопрос о компенсации: %q", m.Text)
	}
	e.press(t, 2000, "rpt:ca:", ":1")
	e.say(2000, "много")
	if got := e.tg.last(); !strings.Contains(got, "Не понял сумму") {
		t.Fatalf("неверная сумма: %q", got)
	}
	e.say(2000, "150 000")
	e.sendPhoto(2000, "RECEIPT1")
	m, _ := e.tg.lastTo(2000)
	if !strings.Contains(m.Text, "Проверьте отчёт") || !strings.Contains(m.Text, "150 000 сум") || !strings.Contains(m.Text, "пропущено") {
		t.Fatalf("итоговый экран: %q", m.Text)
	}
	e.press(t, 2000, "rpt:ok:", "")
	if m, _ := e.tg.lastTo(2000); !strings.Contains(m.Text, "Отчёт отправлен") {
		t.Fatalf("после отправки: %q", m.Text)
	}
	if m, _ := e.tg.lastTo(testAdmin); !strings.Contains(m.Text, "Получен отчёт") || !strings.Contains(m.Text, "150 000 сум") {
		t.Fatalf("админ не получил отчёт: %q", m.Text)
	}
	// Повторно отправить нельзя.
	e.click(2000, "tsk:rp:1")
	if got := e.tg.lastAnswer(); got != "Отчёт по этому заданию уже отправлен." {
		t.Fatalf("повторный отчёт: %q", got)
	}

	// Админ смотрит отчёт (текст + фото) и компенсацию.
	e.click(testAdmin, "adm:rv:1")
	if got := e.tg.last(); !strings.Contains(got, "Оценка сервиса") || !strings.Contains(got, "<b>5</b>") || !strings.Contains(got, "да") {
		t.Fatalf("экран отчёта: %q", got)
	}
	e.click(testAdmin, "adm:cp:w:0")
	if got := e.tg.last(); !strings.Contains(got, "150 000 сум") {
		t.Fatalf("список компенсаций: %q", got)
	}
	e.click(testAdmin, "adm:cc:1")
	if m, _ := e.tg.lastTo(testAdmin); m.Method != "editMessageText" && m.Method != "sendPhoto" {
		t.Fatalf("метод: %s", m.Method)
	}
	e.click(testAdmin, "adm:cpd:1")
	if got := e.tg.last(); !strings.Contains(got, "Подтвердите") {
		t.Fatalf("нет подтверждения выплаты: %q", got)
	}
	e.click(testAdmin, "adm:cpy:1")
	if m, _ := e.tg.lastTo(2000); !strings.Contains(m.Text, "выплачена") {
		t.Fatalf("покупатель не уведомлён о выплате: %q", m.Text)
	}
	e.click(testAdmin, "adm:cp:w:0")
	if got := e.tg.last(); !strings.Contains(got, "Записей: 0") {
		t.Fatalf("после выплаты список к выплате должен быть пуст: %q", got)
	}
	// Выполненное задание засчитало использование: AAA111 свободен и исчерпан (лимит 1), BBB222 ещё занят.
	e.click(testAdmin, "adm:prl:0")
	if got := e.tg.last(); !strings.Contains(got, "0 из 1") || !strings.Contains(got, "#2") {
		t.Fatalf("таблица после выполнения: %q", got)
	}
	// Задание можно отметить проверенным.
	e.click(testAdmin, "adm:rr:1:ok")
	e.click(testAdmin, "adm:tc:1")
	if got := e.tg.last(); !strings.Contains(got, "проверено") {
		t.Fatalf("карточка после проверки: %q", got)
	}
}

// Пустой пул: принятие проходит, админ предупреждён, код выдаётся кнопкой после пополнения.
func TestEmptyPoolThenGetPromo(t *testing.T) {
	e := newTestEnv(t)
	e.addBuyer(t, 2000, "Алия")
	e.upload(testAdmin, "s.yaml", reportFlowScenario)
	for _, d := range []string{"adm:as:0", "adm:as:s:1", "adm:as:d:3", "adm:as:t:2000:0", "adm:as:go"} {
		e.click(testAdmin, d)
	}
	e.click(2000, "tsk:ac:1")
	if got := e.tg.last(); !strings.Contains(got, "принято") || !strings.Contains(got, "Промокод пока недоступен") {
		t.Fatalf("карточка при пустом пуле: %q", got)
	}
	e.click(2000, "tsk:pc:1")
	if got := e.tg.lastAnswer(); got != "Промокодов пока нет. Попробуйте позже." {
		t.Fatalf("кнопка при пустом пуле: %q", got)
	}
	e.click(testAdmin, "adm:pra")
	e.say(testAdmin, "ZZZ999")
	e.click(2000, "tsk:pc:1")
	if got := e.tg.last(); !strings.Contains(got, "ZZZ999") {
		t.Fatalf("код после пополнения: %q", got)
	}
}

// Удаление промокодов из бота: по списку и все свободные, с подтверждением.
func TestPromoDeleteFlow(t *testing.T) {
	e := newTestEnv(t)
	e.click(testAdmin, "adm:pra")
	e.say(testAdmin, "AAA111\nBBB222\nCCC333")

	e.click(testAdmin, "adm:prdl")
	e.say(testAdmin, "BBB222\nNOPE999")
	if got := e.tg.last(); !strings.Contains(got, "Удалено кодов: <b>1</b>") || !strings.Contains(got, "Не удалено") {
		t.Fatalf("удаление по списку: %q", got)
	}
	e.click(testAdmin, "adm:prda")
	if got := e.tg.last(); !strings.Contains(got, "Удалить все незанятые промокоды (2 шт.)") {
		t.Fatalf("подтверждение: %q", got)
	}
	e.click(testAdmin, "adm:prdy")
	if got := e.tg.last(); !strings.Contains(got, "Доступно для выдачи: <b>0</b>") {
		t.Fatalf("после удаления всех: %q", got)
	}
}

// Удаление и отмена заданий из админки.
func TestTaskDeleteAndCancelFlow(t *testing.T) {
	e := newTestEnv(t)
	e.addBuyer(t, 2000, "Алия")
	e.upload(testAdmin, "s.yaml", flowScenario)
	assign := func() {
		for _, d := range []string{"adm:as:0", "adm:as:s:1", "adm:as:d:3", "adm:as:t:2000:0", "adm:as:go"} {
			e.click(testAdmin, d)
		}
	}

	// Не принятое: кнопка удаления с подтверждением.
	assign()
	e.click(testAdmin, "adm:tc:1")
	m, _ := e.tg.lastTo(testAdmin)
	if !strings.Contains(m.Markup, "adm:tdl:1") || strings.Contains(m.Markup, "adm:tcl:1") {
		t.Fatalf("для отправленного нужна кнопка удаления: %s", m.Markup)
	}
	e.click(testAdmin, "adm:tdl:1")
	if got := e.tg.last(); !strings.Contains(got, "Восстановить нельзя") {
		t.Fatalf("нет подтверждения: %q", got)
	}
	e.click(testAdmin, "adm:tdy:1")
	if got := e.tg.lastAnswer(); got != "Задание удалено" {
		t.Fatalf("удаление: %q", got)
	}
	// Старые кнопки в чате покупателя после удаления не работают.
	e.click(2000, "tsk:ac:1")
	if got := e.tg.lastAnswer(); got != "Задание не найдено." {
		t.Fatalf("кнопка удалённого задания: %q", got)
	}

	// Принятое: отмена, покупатель уведомлён.
	assign()
	e.click(2000, "tsk:ac:2")
	e.click(testAdmin, "adm:tc:2")
	m, _ = e.tg.lastTo(testAdmin)
	if !strings.Contains(m.Markup, "adm:tcl:2") || strings.Contains(m.Markup, "adm:tdl:2") {
		t.Fatalf("для принятого нужна кнопка отмены: %s", m.Markup)
	}
	e.click(testAdmin, "adm:tcl:2")
	e.click(testAdmin, "adm:tcy:2")
	if got := e.tg.last(); !strings.Contains(got, "отменено") {
		t.Fatalf("карточка после отмены: %q", got)
	}
	if n, _ := e.tg.lastTo(2000); !strings.Contains(n.Text, "отменено администратором") {
		t.Fatalf("покупатель не уведомлён: %q", n.Text)
	}
	e.click(2000, "tsk:rp:2")
	if got := e.tg.lastAnswer(); !strings.Contains(got, "отменено") {
		t.Fatalf("отчёт по отменённому: %q", got)
	}
}

// completeReport проходит мастер отчёта для reportFlowScenario без компенсации.
func (e *testEnv) completeReport(t *testing.T, user, taskID int64) {
	t.Helper()
	e.click(user, fmt.Sprintf("tsk:rp:%d", taskID))
	e.press(t, user, "rpt:r:", ":5")
	e.press(t, user, "rpt:y:", ":yes")
	e.sendPhoto(user, "P")
	e.press(t, user, "rpt:s:", "")
	e.press(t, user, "rpt:ca:", ":0")
	e.press(t, user, "rpt:ok:", "")
}

// Многоразовый код и предупреждения админу: все коды заняты; коды закончились.
func TestPromoReuseWarningsAndTable(t *testing.T) {
	e := newTestEnvUses(t, 2) // каждый код можно использовать дважды
	e.addBuyer(t, 2000, "Алия")
	e.addBuyer(t, 2001, "Борис")
	e.upload(testAdmin, "s.yaml", reportFlowScenario)
	e.click(testAdmin, "adm:pra")
	e.say(testAdmin, "ONLY001")

	assign := func(user int64) {
		for _, d := range []string{"adm:as:0", "adm:as:s:1", "adm:as:d:3", fmt.Sprintf("adm:as:t:%d:0", user), "adm:as:go"} {
			e.click(testAdmin, d)
		}
	}
	assign(2000)
	e.click(2000, "tsk:ac:1")
	if got := e.tg.last(); !strings.Contains(got, "ONLY001") {
		t.Fatalf("первый покупатель без кода: %q", got)
	}

	// Второй покупатель: единственный код занят, использования ещё есть.
	assign(2001)
	e.click(2001, "tsk:ac:2")
	if got := e.tg.last(); !strings.Contains(got, "Промокод пока недоступен") {
		t.Fatalf("карточка без кода: %q", got)
	}
	if !e.tg.anyTo(testAdmin, "Все промокоды заняты активными заданиями") {
		t.Fatal("нет предупреждения «все коды заняты»")
	}

	// Таблица: код закреплён за заданием #1, осталось 2 из 2.
	e.click(testAdmin, "adm:prl:0")
	if got := e.tg.last(); !strings.Contains(got, "<pre>") || !strings.Contains(got, "ONLY001") || !strings.Contains(got, "2 из 2") || !strings.Contains(got, "#1") {
		t.Fatalf("таблица занятого кода: %q", got)
	}
	e.click(testAdmin, "adm:pr")
	if got := e.tg.last(); !strings.Contains(got, "Использований на код: <b>2</b>") || !strings.Contains(got, "Все коды сейчас заняты") {
		t.Fatalf("сводка: %q", got)
	}

	// Занятый код удалить нельзя.
	e.click(testAdmin, "adm:prdl")
	e.say(testAdmin, "ONLY001")
	if got := e.tg.last(); !strings.Contains(got, "Удалено кодов: <b>0</b>") {
		t.Fatalf("удаление занятого: %q", got)
	}

	// Первое задание выполнено: код свободен (1 из 2), без предупреждения об исчерпании.
	e.completeReport(t, 2000, 1)
	e.click(testAdmin, "adm:prl:0")
	if got := e.tg.last(); !strings.Contains(got, "1 из 2") || !strings.Contains(got, " нет") {
		t.Fatalf("таблица после выполнения: %q", got)
	}
	if e.tg.anyTo(testAdmin, "Не осталось ни одного промокода") {
		t.Fatal("рано предупреждать об исчерпании: осталось одно использование")
	}

	// Второй покупатель забирает код по кнопке и выполняет задание: коды закончились.
	e.click(2001, "tsk:pc:2")
	if got := e.tg.last(); !strings.Contains(got, "ONLY001") {
		t.Fatalf("код по кнопке: %q", got)
	}
	e.completeReport(t, 2001, 2)
	if !e.tg.anyTo(testAdmin, "Не осталось ни одного промокода со свободными использованиями") {
		t.Fatal("нет предупреждения об исчерпании кодов")
	}
	e.click(testAdmin, "adm:pr")
	if got := e.tg.last(); !strings.Contains(got, "Нет ни одного кода со свободными использованиями") || !strings.Contains(got, "Исчерпаны: 1") {
		t.Fatalf("сводка после исчерпания: %q", got)
	}
}

// clickOnPhoto нажатие кнопки под сообщением-фото (карточка компенсации).
func (e *testEnv) clickOnPhoto(from int64, data string) {
	e.b.ProcessUpdate(context.Background(), &models.Update{ID: 1, CallbackQuery: &models.CallbackQuery{
		ID: "cb", From: models.User{ID: from, FirstName: "Тест"}, Data: data,
		Message: models.MaybeInaccessibleMessage{
			Type: models.MaybeInaccessibleMessageTypeMessage,
			Message: &models.Message{ID: 11, Chat: models.Chat{ID: from, Type: models.ChatTypePrivate},
				Photo: []models.PhotoSize{{FileID: "x"}}},
		},
	}})
}

// lastMethods методы последних n сообщений чату (от старых к новым).
func (f *fakeTG) methodsTo(chat int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.sent {
		if m.Chat == chat {
			out = append(out, m.Method)
		}
	}
	return out
}

// completeReportWithComp проходит мастер для reportFlowScenario с компенсацией.
func (e *testEnv) completeReportWithComp(t *testing.T, user, taskID int64, amount string) {
	t.Helper()
	e.click(user, fmt.Sprintf("tsk:rp:%d", taskID))
	e.press(t, user, "rpt:r:", ":5")
	e.press(t, user, "rpt:y:", ":yes")
	e.sendPhoto(user, "P-"+amount)
	e.press(t, user, "rpt:s:", "")
	e.press(t, user, "rpt:ca:", ":1")
	e.say(user, amount)
	e.sendPhoto(user, "RECEIPT-"+amount)
	e.press(t, user, "rpt:ok:", "")
}

func (e *testEnv) assignTo(user int64, scenarioID int) {
	for _, d := range []string{"adm:as:0", fmt.Sprintf("adm:as:s:%d", scenarioID), "adm:as:d:3", fmt.Sprintf("adm:as:t:%d:0", user), "adm:as:go"} {
		e.click(testAdmin, d)
	}
}

// Возврат отчёта на доработку с комментарием и повторная отправка.
func TestReworkFlow(t *testing.T) {
	e := newTestEnv(t)
	e.addBuyer(t, 2000, "Алия")
	e.upload(testAdmin, "s.yaml", reportFlowScenario)
	e.assignTo(2000, 1)
	e.click(2000, "tsk:ac:1")
	e.completeReportWithComp(t, 2000, 1, "150000")

	// Кнопки решения есть в сообщении отчёта.
	e.click(testAdmin, "adm:rv:1")
	m, _ := e.tg.lastTo(testAdmin)
	if !strings.Contains(m.Markup, "adm:rr:1:ok") || !strings.Contains(m.Markup, "adm:rw:1") {
		t.Fatalf("нет кнопок решения: %s", m.Markup)
	}
	// Отмена возврата ничего не меняет.
	e.click(testAdmin, "adm:rw:1")
	if got := e.tg.last(); !strings.Contains(got, "Напишите комментарий") {
		t.Fatalf("запрос комментария: %q", got)
	}
	e.click(testAdmin, "adm:rwx:1")
	e.say(testAdmin, "этот текст не должен уйти покупателю")
	if e.tg.anyTo(2000, "не должен уйти") {
		t.Fatal("после отмены комментарий не должен отправляться покупателю")
	}

	// Возврат со свободным комментарием.
	e.click(testAdmin, "adm:rw:1")
	e.say(testAdmin, "Фото посылки размыто, переснимите")
	n, _ := e.tg.lastTo(2000)
	if !strings.Contains(n.Text, "возвращён на доработку") || !strings.Contains(n.Text, "Фото посылки размыто") || !strings.Contains(n.Markup, "tsk:rp:1") {
		t.Fatalf("покупатель не получил комментарий: %+v", n)
	}
	e.click(2000, "tsk:v:1")
	if got := e.tg.last(); !strings.Contains(got, "на доработке") || !strings.Contains(got, "Фото посылки размыто") {
		t.Fatalf("карточка у покупателя: %q", got)
	}
	// Повторно вернуть нельзя, отчёт теперь у покупателя.
	e.click(testAdmin, "adm:rw:1")
	if got := e.tg.lastAnswer(); !strings.Contains(got, "ожидающий проверки") {
		t.Fatalf("повторный возврат: %q", got)
	}

	// Покупатель исправляет: сначала напоминание комментария, затем вопросы заново.
	e.click(2000, "tsk:rp:1")
	if !e.tg.anyTo(2000, "Комментарий администратора к отчёту") {
		t.Fatal("перед доработкой нужно показать комментарий")
	}
	e.press(t, 2000, "rpt:r:", ":4")
	e.press(t, 2000, "rpt:y:", ":yes")
	e.sendPhoto(2000, "PHOTO-FIXED")
	e.press(t, 2000, "rpt:s:", "")
	// Компенсация уже отправлена: заново её не спрашиваем, сразу итог с прежней суммой.
	if m, _ := e.tg.lastTo(2000); !strings.Contains(m.Text, "Проверьте отчёт") || !strings.Contains(m.Text, "150 000 сум, данные уже отправлены") {
		t.Fatalf("итог без повторного ввода компенсации: %q", m.Text)
	}
	e.press(t, 2000, "rpt:ok:", "")
	if !e.tg.anyTo(testAdmin, "версия 2") && !e.tg.anyTo(testAdmin, "Получен отчёт") {
		t.Fatal("админ не получил исправленный отчёт")
	}
	e.click(testAdmin, "adm:rv:1")
	if got := e.tg.last(); !strings.Contains(got, "версия 2") || !strings.Contains(got, "150 000 сум (к выплате)") {
		t.Fatalf("новая версия отчёта: %q", got)
	}
	// Принимаем отчёт: он остаётся в «Отчётах», пока компенсация не выплачена.
	e.click(testAdmin, "adm:rr:1:ok")
	e.click(testAdmin, "adm:tk:r:0")
	if got := e.tg.last(); !strings.Contains(got, "найдено: 1") {
		t.Fatalf("принятый, но не выплаченный отчёт должен оставаться в «Отчётах»: %q", got)
	}
	e.click(testAdmin, "adm:tk:f:0")
	if got := e.tg.last(); !strings.Contains(got, "найдено: 0") {
		t.Fatalf("не закрытое задание не должно быть в «Закрыто»: %q", got)
	}
	e.click(testAdmin, "adm:cpd:1")
	e.click(testAdmin, "adm:cpy:1")
	e.click(testAdmin, "adm:tk:r:0")
	if got := e.tg.last(); !strings.Contains(got, "найдено: 0") {
		t.Fatalf("после выплаты задание уходит из «Отчётов»: %q", got)
	}
	e.click(testAdmin, "adm:tk:f:0")
	if got := e.tg.last(); !strings.Contains(got, "найдено: 1") {
		t.Fatalf("после выплаты задание в «Закрыто»: %q", got)
	}
}

// Отклонение компенсации: кнопки прямо под фото чека, комментарий, исправление покупателем.
func TestCompensationRejectFlow(t *testing.T) {
	e := newTestEnv(t)
	e.addBuyer(t, 2000, "Алия")
	e.upload(testAdmin, "s.yaml", reportFlowScenario)
	e.assignTo(2000, 1)
	e.click(2000, "tsk:ac:1")
	e.completeReportWithComp(t, 2000, 1, "150000")

	// Компенсация открывается ОДНИМ сообщением: фото чека, а кнопки под ним.
	before := len(e.tg.methodsTo(testAdmin))
	e.click(testAdmin, "adm:cc:1")
	ms := e.tg.methodsTo(testAdmin)[before:]
	if len(ms) != 1 || ms[0] != "sendPhoto" {
		t.Fatalf("карточка компенсации должна быть одним сообщением-фото, получили %v", ms)
	}
	m, _ := e.tg.lastTo(testAdmin)
	if !strings.Contains(m.Markup, "adm:cpd:1") || !strings.Contains(m.Markup, "adm:crj:1") || !strings.Contains(m.Text, "150 000 сум") {
		t.Fatalf("кнопки под фото чека: %+v", m)
	}

	// Действия под фото меняют подпись, а не текст.
	e.clickOnPhoto(testAdmin, "adm:crj:1")
	if last := e.tg.lastMsg(); last.Method != "editMessageCaption" || !strings.Contains(last.Text, "Напишите комментарий") {
		t.Fatalf("запрос комментария должен редактировать подпись: %+v", last)
	}
	e.say(testAdmin, "Сумма на чеке другая")
	n, _ := e.tg.lastTo(2000)
	if !strings.Contains(n.Text, "Компенсация") || !strings.Contains(n.Text, "Сумма на чеке другая") || !strings.Contains(n.Markup, "tsk:cf:1") {
		t.Fatalf("покупатель не получил комментарий: %+v", n)
	}
	// Выплатить отклонённую нельзя.
	e.click(testAdmin, "adm:cpd:1")
	if got := e.tg.lastAnswer(); !strings.Contains(got, "Уже обработано") {
		t.Fatalf("выплата отклонённой: %q", got)
	}
	e.click(testAdmin, "adm:cp:r:0")
	if got := e.tg.last(); !strings.Contains(got, "Компенсации: отклонено, ждём исправления") || !strings.Contains(got, "Записей: 1") {
		t.Fatalf("список отклонённых: %q", got)
	}

	// Покупатель исправляет сумму и чек.
	e.click(2000, "tsk:v:1")
	if got := e.tg.last(); !strings.Contains(got, "Сумма на чеке другая") {
		t.Fatalf("комментарий не виден в карточке: %q", got)
	}
	e.click(2000, "tsk:cf:1")
	if !e.tg.anyTo(2000, "Введите сумму") {
		t.Fatal("нет запроса суммы")
	}
	e.say(2000, "120 000")
	e.sendPhoto(2000, "RECEIPT-NEW")
	m, _ = e.tg.lastTo(2000)
	if !strings.Contains(m.Text, "120 000 сум") {
		t.Fatalf("подтверждение: %q", m.Text)
	}
	e.press(t, 2000, "rpt:ok:", "")
	if !e.tg.anyTo(2000, "Исправленные данные компенсации отправлены") {
		t.Fatal("нет подтверждения покупателю")
	}
	if !e.tg.anyTo(testAdmin, "исправил данные компенсации") {
		t.Fatal("админ не уведомлён об исправлении")
	}
	e.click(testAdmin, "adm:cp:w:0")
	if got := e.tg.last(); !strings.Contains(got, "Записей: 1") || !strings.Contains(got, "120 000") {
		t.Fatalf("исправленная компенсация снова к выплате: %q", got)
	}
	// Теперь выплата проходит.
	e.click(testAdmin, "adm:cpy:1")
	if m, _ := e.tg.lastTo(2000); !strings.Contains(m.Text, "выплачена") {
		t.Fatalf("уведомление о выплате: %q", m.Text)
	}
}

const albumScenario = `key: album
title: Много фото
operator: BTS
steps: [Шаг]
questions:
  - {key: p1, text: Фото один, type: photo}
  - {key: p2, text: Фото два, type: photo}
  - {key: p3, text: Фото три, type: photo}
  - {key: note, text: Комментарий, type: text}
`

// Фото отчёта приходят альбомом, а сообщение с кнопками решения всегда последнее.
func TestReportMediaBeforeButtons(t *testing.T) {
	e := newTestEnv(t)
	e.addBuyer(t, 2000, "Алия")
	e.upload(testAdmin, "album.yaml", albumScenario)
	e.assignTo(2000, 1)
	e.click(2000, "tsk:ac:1")
	e.click(2000, "tsk:rp:1")
	e.sendPhoto(2000, "A1")
	e.sendPhoto(2000, "A2")
	e.sendPhoto(2000, "A3")
	e.say(2000, "всё хорошо")
	e.press(t, 2000, "rpt:ca:", ":0")
	e.press(t, 2000, "rpt:ok:", "")

	before := len(e.tg.methodsTo(testAdmin))
	e.click(testAdmin, "adm:rv:1")
	ms := e.tg.methodsTo(testAdmin)[before:]
	if len(ms) != 2 || ms[0] != "sendMediaGroup" || ms[1] != "sendMessage" {
		t.Fatalf("ожидали альбом, затем текст с кнопками; получили %v", ms)
	}
	m, _ := e.tg.lastTo(testAdmin)
	if !strings.Contains(m.Markup, "adm:rr:1:ok") {
		t.Fatalf("кнопки должны быть в последнем сообщении: %+v", m)
	}
}

// Один и тот же сценарий можно назначать одному покупателю повторно.
func TestAssignSameScenarioTwice(t *testing.T) {
	e := newTestEnv(t)
	e.addBuyer(t, 2000, "Алия")
	e.upload(testAdmin, "s.yaml", reportFlowScenario)
	e.assignTo(2000, 1)
	if got := e.tg.last(); !strings.Contains(got, "Отправлено: 1") {
		t.Fatalf("первое назначение: %q", got)
	}
	e.assignTo(2000, 1) // первое ещё не принято
	got := e.tg.last()
	if !strings.Contains(got, "Отправлено: 1") || !strings.Contains(got, "выдано ещё одно") {
		t.Fatalf("повторное назначение должно пройти с пометкой: %q", got)
	}
	// У покупателя два задания; в списке их легко различить по номерам.
	e.click(2000, "tsk:l")
	m, _ := e.tg.lastTo(2000)
	if !strings.Contains(m.Markup, "#1 ") || !strings.Contains(m.Markup, "#2 ") {
		t.Fatalf("список заданий покупателя: %s", m.Markup)
	}
}

// Можно передумать и изменить уже отправленную компенсацию прямо при доработке отчёта.
func TestReworkChangeCompensation(t *testing.T) {
	e := newTestEnv(t)
	e.addBuyer(t, 2000, "Алия")
	e.upload(testAdmin, "s.yaml", reportFlowScenario)
	e.assignTo(2000, 1)
	e.click(2000, "tsk:ac:1")
	e.completeReportWithComp(t, 2000, 1, "150000")
	e.click(testAdmin, "adm:rw:1")
	e.say(testAdmin, "Поправьте ответы")

	e.click(2000, "tsk:rp:1")
	e.press(t, 2000, "rpt:r:", ":3")
	e.press(t, 2000, "rpt:y:", ":no")
	e.sendPhoto(2000, "P2")
	e.press(t, 2000, "rpt:s:", "")
	// Передумал: открываем ввод, затем возвращаемся «Назад» к итогу без изменений.
	e.press(t, 2000, "rpt:cc:", "")
	if !e.tg.anyTo(2000, "Введите сумму") {
		t.Fatal("нет запроса новой суммы")
	}
	e.press(t, 2000, "rpt:b:", "")
	if m, _ := e.tg.lastTo(2000); !strings.Contains(m.Text, "останутся без изменений") {
		t.Fatalf("после «Назад» компенсация должна остаться прежней: %q", m.Text)
	}
	// Теперь меняем по-настоящему.
	e.press(t, 2000, "rpt:cc:", "")
	e.say(2000, "99 000")
	e.sendPhoto(2000, "RECEIPT-99")
	if m, _ := e.tg.lastTo(2000); !strings.Contains(m.Text, "99 000 сум, чек приложен") {
		t.Fatalf("итог с новой суммой: %q", m.Text)
	}
	e.press(t, 2000, "rpt:ok:", "")
	e.click(testAdmin, "adm:cp:w:0")
	if got := e.tg.last(); !strings.Contains(got, "99 000") || !strings.Contains(got, "Записей: 1") {
		t.Fatalf("компенсация должна обновиться, а не задвоиться: %q", got)
	}
}

// Если компенсацию отклонили и отчёт вернули на доработку, новые данные компенсации обязательны.
func TestReworkAfterRejectedCompensation(t *testing.T) {
	e := newTestEnv(t)
	e.addBuyer(t, 2000, "Алия")
	e.upload(testAdmin, "s.yaml", reportFlowScenario)
	e.assignTo(2000, 1)
	e.click(2000, "tsk:ac:1")
	e.completeReportWithComp(t, 2000, 1, "150000")
	e.click(testAdmin, "adm:cc:1")
	e.clickOnPhoto(testAdmin, "adm:crj:1")
	e.say(testAdmin, "Чек не тот")
	e.click(testAdmin, "adm:rw:1")
	e.say(testAdmin, "И ответы поправьте")

	e.click(2000, "tsk:rp:1")
	e.press(t, 2000, "rpt:r:", ":5")
	e.press(t, 2000, "rpt:y:", ":yes")
	e.sendPhoto(2000, "P3")
	e.press(t, 2000, "rpt:s:", "")
	// Вопрос «нужна ли компенсация» не задаётся: сразу просят новую сумму.
	if m, _ := e.tg.lastTo(2000); !strings.Contains(m.Text, "Введите сумму") {
		t.Fatalf("ожидали запрос суммы: %q", m.Text)
	}
	e.say(2000, "140000")
	e.sendPhoto(2000, "RECEIPT-140")
	e.press(t, 2000, "rpt:ok:", "")
	e.click(testAdmin, "adm:cp:w:0")
	if got := e.tg.last(); !strings.Contains(got, "140 000") || !strings.Contains(got, "Записей: 1") {
		t.Fatalf("исправленная компенсация снова к выплате: %q", got)
	}
}

// shift «старит» задание: сдвигает его отметки времени в прошлое, имитируя ход времени без ожидания.
func (e *testEnv) shift(t *testing.T, taskID int64, d time.Duration) {
	t.Helper()
	sec := int64(d.Seconds())
	if _, err := e.store.DB().Exec(`UPDATE tasks SET
		sent_at     = CASE WHEN sent_at     > 0 THEN sent_at     - ?1 ELSE 0 END,
		accepted_at = CASE WHEN accepted_at > 0 THEN accepted_at - ?1 ELSE 0 END,
		due_at      = CASE WHEN due_at      > 0 THEN due_at      - ?1 ELSE 0 END
		WHERE id = ?2`, sec, taskID); err != nil {
		t.Fatal(err)
	}
	// Журнал напоминаний привязан к базовому моменту: сдвигаем его вместе с заданием,
	// как будто прошло время, а не изменилась дата отправки.
	if _, err := e.store.DB().Exec(`UPDATE task_reminders SET base_at = base_at - ?1, sent_at = sent_at - ?1 WHERE task_id = ?2`, sec, taskID); err != nil {
		t.Fatal(err)
	}
}

// quietOff отключает тихие часы, чтобы результат тестов не зависел от времени суток запуска.
func (e *testEnv) quietOff(t *testing.T) {
	t.Helper()
	e.app.reminders = service.NewReminders(e.store, e.app.tasks, service.RemindConfig{
		Accept: service.MinutesToDurations([]int{1440, 2880}), Report: service.MinutesToDurations([]int{1440, 2880}),
		Stale: 24 * time.Hour,
	}, time.UTC)
}

func (e *testEnv) runReminders(t *testing.T) {
	t.Helper()
	if err := e.app.RunReminders(context.Background(), e.b); err != nil {
		t.Fatal(err)
	}
}

// count сколько сообщений чату содержат подстроку.
func (f *fakeTG) count(chat int64, substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.sent {
		if m.Chat == chat && strings.Contains(m.Text, substr) {
			n++
		}
	}
	return n
}

// Напоминания покупателю по ходу времени. Админу ничего не отправляется: он смотрит отметки в списке.
func TestReminderFlow(t *testing.T) {
	e := newTestEnv(t)
	e.addBuyer(t, 2000, "Алия")
	e.quietOff(t)
	e.upload(testAdmin, "s.yaml", reportFlowScenario)
	e.assignTo(2000, 1) // отправлено сейчас

	e.runReminders(t)
	if e.tg.count(2000, "Напоминание") != 0 {
		t.Fatal("сразу после отправки напоминать рано")
	}
	e.shift(t, 1, 25*time.Hour)
	e.runReminders(t)
	if e.tg.count(2000, "ещё не ответили на задание") != 1 {
		t.Fatal("нет первого напоминания через 24 ч")
	}
	m, _ := e.tg.lastTo(2000)
	if !strings.Contains(m.Markup, "tsk:v:1") {
		t.Fatalf("в напоминании нет кнопки открытия: %s", m.Markup)
	}
	e.runReminders(t) // повторный проход ничего не дублирует
	if e.tg.count(2000, "ещё не ответили") != 1 {
		t.Fatal("напоминание продублировалось")
	}
	e.shift(t, 1, 24*time.Hour) // всего 49 ч
	e.runReminders(t)
	if e.tg.count(2000, "ещё не ответили") != 2 {
		t.Fatal("нет второго напоминания через 48 ч")
	}
	e.shift(t, 1, 24*time.Hour) // 73 ч: эскалации админу больше нет
	e.runReminders(t)
	if e.tg.count(testAdmin, "не ответил") != 0 || e.tg.count(testAdmin, "Покупатель") != 0 {
		t.Fatal("админу не должны приходить уведомления о молчании покупателя")
	}
	if e.tg.count(2000, "ещё не ответили") != 2 {
		t.Fatal("после второго напоминания новых быть не должно")
	}
	// Принятие останавливает напоминания о принятии.
	e.click(2000, "tsk:ac:1")
	before := e.tg.count(2000, "ещё не ответили")
	e.shift(t, 1, 100*time.Hour)
	e.runReminders(t)
	if e.tg.count(2000, "ещё не ответили") != before {
		t.Fatal("после принятия напоминаний о принятии быть не должно")
	}
}

// Автоматическая просрочка: статус, уведомления покупателю и админу, вкладка «Просрочено».
func TestAutoExpiry(t *testing.T) {
	e := newTestEnv(t)
	e.addBuyer(t, 2000, "Алия")
	e.quietOff(t)
	e.upload(testAdmin, "s.yaml", reportFlowScenario)
	e.assignTo(2000, 1)
	e.click(2000, "tsk:ac:1")

	e.shift(t, 1, 71*time.Hour) // срок 3 дня: ещё не вышел
	e.runReminders(t)
	if e.tg.anyTo(2000, "Срок по заданию") {
		t.Fatal("рано объявлять просрочку")
	}
	e.shift(t, 1, 2*time.Hour)
	e.runReminders(t)
	if !e.tg.anyTo(2000, "Срок по заданию «Полный отчёт» вышел") {
		t.Fatal("покупатель не получил уведомление о просрочке")
	}
	m, _ := e.tg.lastTo(2000)
	if !strings.Contains(m.Markup, "tsk:rp:1") {
		t.Fatalf("в уведомлении о просрочке нет кнопки отчёта: %s", m.Markup)
	}
	if e.tg.anyTo(testAdmin, "просрочено") {
		t.Fatal("админу не отправляем отдельное уведомление: просрочка видна в списке")
	}
	e.click(testAdmin, "adm:tk:o:0")
	if got := e.tg.last(); !strings.Contains(got, "найдено: 1") {
		t.Fatalf("вкладка «Просрочено»: %q", got)
	}
	if m, _ := e.tg.lastTo(testAdmin); !strings.Contains(m.Markup, "⚠ #1") || !strings.Contains(m.Markup, "просрочено на") {
		t.Fatalf("в списке нет отметки о просрочке: %s", m.Markup)
	}
	// Опоздавший отчёт принимается (с пометкой).
	e.completeReportWithComp(t, 2000, 1, "50000")
	if !e.tg.anyTo(testAdmin, "после срока") {
		t.Fatal("отчёт после срока должен быть помечен")
	}
	// Повторных уведомлений о просрочке нет.
	e.runReminders(t)
	if e.tg.count(2000, "Срок по заданию") != 1 {
		t.Fatal("уведомление о просрочке продублировалось")
	}
}

func TestManualPing(t *testing.T) {
	e := newTestEnv(t)
	e.addBuyer(t, 2000, "Алия")
	e.upload(testAdmin, "s.yaml", reportFlowScenario)
	e.assignTo(2000, 1)

	e.click(testAdmin, "adm:tc:1")
	m, _ := e.tg.lastTo(testAdmin)
	if !strings.Contains(m.Markup, "adm:tp:1") {
		t.Fatalf("в карточке нет кнопки напоминания: %s", m.Markup)
	}
	e.click(testAdmin, "adm:tp:1")
	if got := e.tg.lastAnswer(); got != "Напоминание отправлено" {
		t.Fatalf("пинг: %q", got)
	}
	if !e.tg.anyTo(2000, "ещё не ответили на задание") || e.tg.anyTo(2000, "Администратор") {
		t.Fatal("покупатель должен получить обычное напоминание без приписки про администратора")
	}
	e.click(testAdmin, "adm:tp:1")
	if got := e.tg.lastAnswer(); !strings.Contains(got, "уже отправлено") {
		t.Fatalf("повторный пинг сразу: %q", got)
	}
	// Пинг по покупателю целиком.
	e.click(testAdmin, "adm:up:2000")
	if got := e.tg.lastAnswer(); !strings.Contains(got, "Недавно уже напоминали") {
		t.Fatalf("пинг покупателя: %q", got)
	}
	// Удалённое/завершённое: напоминать нечего.
	e.click(2000, "tsk:dy:1")
	e.click(testAdmin, "adm:tp:1")
	if got := e.tg.lastAnswer(); !strings.Contains(got, "напоминать не нужно") {
		t.Fatalf("пинг по отказавшемуся: %q", got)
	}
}

func TestSettingsScreen(t *testing.T) {
	e := newTestEnv(t)
	e.addBuyer(t, 2000, "Алия")
	e.click(testAdmin, "adm:st")
	got := e.tg.last()
	if !strings.Contains(got, "Бэкап базы: ежедневно в 03:00") || strings.Contains(got, "Эскалация") {
		t.Fatalf("экран настроек: %q", got)
	}
	if m, _ := e.tg.lastTo(testAdmin); strings.Contains(m.Markup, "adm:ste:accept") || strings.Contains(m.Markup, "adm:sts") {
		t.Fatalf("настроек напоминаний в боте больше нет: %s", m.Markup)
	}
	// Время бэкапа: неверное значение, верное, отмена ввода.
	e.click(testAdmin, "adm:ste:backup")
	e.say(testAdmin, "99:99")
	if got := e.tg.last(); !strings.Contains(got, "❌") {
		t.Fatalf("неверное значение: %q", got)
	}
	e.say(testAdmin, "4:30")
	if got := e.tg.last(); !strings.Contains(got, "Сохранено") || !strings.Contains(got, "04:30") {
		t.Fatalf("сохранение: %q", got)
	}
	e.click(testAdmin, "adm:ste:backup")
	e.click(testAdmin, "adm:stc")
	e.say(testAdmin, "off") // после отмены это обычный текст, настройка не меняется
	e.click(testAdmin, "adm:st")
	if got := e.tg.last(); !strings.Contains(got, "04:30") {
		t.Fatalf("после отмены время бэкапа не должно измениться: %q", got)
	}
	// Покупателю настройки недоступны.
	e.click(2000, "adm:st")
	if got := e.tg.lastAnswer(); got != "Нет доступа." {
		t.Fatalf("настройки у покупателя: %q", got)
	}
}

func TestBackupFlow(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	// Бэкап по кнопке отправляется нажавшему админу файлом.
	e.click(testAdmin, "adm:stb")
	var doc sentMsg
	e.tg.mu.Lock()
	for _, m := range e.tg.sent {
		if m.Method == "sendDocument" {
			doc = m
		}
	}
	e.tg.mu.Unlock()
	if doc.Chat != testAdmin || !strings.HasSuffix(doc.FileName, ".db.gz") || doc.FileSize == 0 || !strings.Contains(doc.Text, "Копия базы по запросу") {
		t.Fatalf("отправленный бэкап: %+v", doc)
	}

	// Ежедневный бэкап: время «00:00» уже наступило, отправляется один раз за день.
	if _, err := e.app.settings.Set(ctx, testAdmin, service.KeyBackupTime, "00:00"); err != nil {
		t.Fatal(err)
	}
	countDocs := func() int {
		n := 0
		e.tg.mu.Lock()
		defer e.tg.mu.Unlock()
		for _, m := range e.tg.sent {
			if m.Method == "sendDocument" {
				n++
			}
		}
		return n
	}
	before := countDocs()
	if err := e.app.RunBackup(ctx, e.b); err != nil {
		t.Fatal(err)
	}
	if countDocs() != before+1 {
		t.Fatal("ежедневный бэкап не отправлен")
	}
	if err := e.app.RunBackup(ctx, e.b); err != nil || countDocs() != before+1 {
		t.Fatal("второй запуск в тот же день не должен делать копию")
	}
	// Отключённый бэкап не выполняется.
	if _, err := e.app.settings.Set(ctx, testAdmin, service.KeyBackupTime, "off"); err != nil {
		t.Fatal(err)
	}
	_ = e.app.settings.SetInternal(ctx, service.KeyLastBackup, "")
	if err := e.app.RunBackup(ctx, e.b); err != nil || countDocs() != before+1 {
		t.Fatal("при отключённом бэкапе копия не делается")
	}
}
