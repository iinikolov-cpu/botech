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
	Method string
	Chat   int64
	Text   string
	Markup string // reply_markup как JSON-строка (содержит callback-данные кнопок)
}

// fakeTG подменяет Telegram API: запоминает отправленное и отдаёт файлы.
type fakeTG struct {
	mu      sync.Mutex
	sent    []sentMsg
	answers []string          // тексты answerCallbackQuery
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
		f.sent = append(f.sent, sentMsg{Method: method, Chat: chat, Text: form["caption"]})
	case "answerCallbackQuery":
		f.answers = append(f.answers, form["text"])
	case "getFile":
		id := form["file_id"]
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"` + id + `","file_path":"` + id + `"}}`))
		return
	}
	f.mu.Unlock()
	_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":1,"type":"private"}}}`))
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
}

func newTestBot(t *testing.T) (*bot.Bot, *fakeTG) {
	e := newTestEnv(t)
	return e.b, e.tg
}

func newTestEnv(t *testing.T) *testEnv {
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

	tasks := service.NewTasks(store)
	app := New(Services{
		Access: access, Scenarios: service.NewScenarios(store), Tasks: tasks, Dialog: service.NewDialog(store),
		Promos: service.NewPromos(store), Reports: service.NewReports(store, tasks), PromoLowThreshold: 2,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.UTC)
	b, err := bot.New("123:TEST",
		bot.WithSkipGetMe(), bot.WithServerURL(srv.URL), bot.WithNotAsyncHandlers(),
		bot.WithMiddlewares(app.Middleware), bot.WithDefaultHandler(app.DefaultHandler),
	)
	if err != nil {
		t.Fatal(err)
	}
	app.Register(b, "testbot")
	return &testEnv{b: b, tg: tg, store: store}
}

// addBuyer создаёт активного покупателя.
func (e *testEnv) addBuyer(t *testing.T, id int64, name string) {
	t.Helper()
	now := time.Now().UTC()
	if err := e.store.Repos().Users.Create(context.Background(), &domain.User{
		TgID: id, Role: domain.RoleBuyer, Status: domain.StatusActive, Lang: "ru", FirstName: name, CreatedAt: now, UpdatedAt: now,
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
	if n, _ := e.tg.lastTo(testAdmin); !strings.Contains(n.Text, "принял задание") {
		t.Fatalf("админ не уведомлён: %q", n.Text)
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
	// Второе принятие: остаток 2 = порогу, админ получает предупреждение.
	assign(2001)
	e.click(2001, "tsk:ac:2")
	if got := e.tg.last(); !strings.Contains(got, "BBB222") {
		t.Fatalf("второй код: %q", got)
	}
	if m, _ := e.tg.lastTo(testAdmin); !strings.Contains(m.Text, "осталось: <b>2</b>") {
		t.Fatalf("нет предупреждения о нехватке: %q", m.Text)
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
	// Задание можно отметить проверенным.
	e.click(testAdmin, "adm:rv:1:ok")
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
