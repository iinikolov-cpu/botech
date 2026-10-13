package handlers

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"botech/internal/domain"
	"botech/internal/service"
)

// captureMail запоминает письма вместо отправки.
type captureMail struct {
	mu   sync.Mutex
	sent []struct{ to, subject, body string }
}

func (c *captureMail) Send(_ context.Context, to, subject, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, struct{ to, subject, body string }{to, subject, body})
	return nil
}

func (c *captureMail) code() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) == 0 {
		return ""
	}
	for _, f := range strings.Fields(c.sent[len(c.sent)-1].body) {
		if len(f) == service.OTPLength && strings.Trim(f, "0123456789") == "" {
			return f
		}
	}
	return ""
}

func (c *captureMail) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

// newEmailEnv окружение с включённым входом по почте.
func newEmailEnv(t *testing.T) (*testEnv, *captureMail) {
	t.Helper()
	e := newTestEnv(t)
	m := &captureMail{}
	e.app.UseEmailAuth(service.NewEmailAuth(e.store, m, []string{"dila-team.com", "listee-team.com"}, 300*time.Second))
	return e, m
}

// Незнакомый сотрудник: почта, код из письма, доступ.
func TestEmailLoginFlow(t *testing.T) {
	e, mail := newEmailEnv(t)
	const stranger = int64(9000)

	e.say(stranger, "/start")
	if got := e.tg.last(); !strings.Contains(got, "рабочую почту") || !strings.Contains(got, "dila-team.com") || !strings.Contains(got, "listee-team.com") {
		t.Fatalf("приглашение ввести почту: %q", got)
	}
	// Чужой домен: письмо не отправляется.
	e.say(stranger, "ivan@gmail.com")
	if got := e.tg.last(); !strings.Contains(got, "Почта не подходит") && !strings.Contains(got, "разрешённом домене") {
		t.Fatalf("чужой домен: %q", got)
	}
	if mail.count() != 0 {
		t.Fatal("на чужой домен письма быть не должно")
	}
	// Код без запроса не принимается и не создаёт пользователя.
	e.say(stranger, "123456")
	if got := e.tg.last(); !strings.Contains(got, "рабочую почту") {
		t.Fatalf("код без запроса: %q", got)
	}

	before := e.tg.deletedCount()
	e.say(stranger, "Ivan.Petrov@Dila-Team.com")
	if mail.count() != 1 {
		t.Fatal("письмо с кодом не отправлено")
	}
	if got := e.tg.last(); !strings.Contains(got, "Код отправлен на <b>ivan.petrov@dila-team.com</b>") || !strings.Contains(got, "5 мин") || !strings.Contains(got, "Спам") {
		t.Fatalf("сообщение об отправке: %q", got)
	}
	if e.tg.deletedCount() <= before {
		t.Fatal("сообщение с почтой должно удаляться из чата")
	}
	if u, _ := e.app.access.Lookup(context.Background(), stranger); u != nil {
		t.Fatal("до ввода кода доступа нет")
	}

	// Неверный код.
	e.say(stranger, "000000")
	if got := e.tg.last(); !strings.Contains(got, "неверный код") || !strings.Contains(got, "осталось попыток: 4") {
		t.Fatalf("неверный код: %q", got)
	}
	// Верный код: доступ открыт, дальше выбор типов заданий, админ уведомлён.
	code := mail.code()
	if code == "" {
		t.Fatal("в письме нет кода")
	}
	before = e.tg.deletedCount()
	e.say(stranger, code[:3]+" "+code[3:]) // допускаем пробел посередине
	u, _ := e.app.access.Lookup(context.Background(), stranger)
	if u == nil || u.Status != domain.StatusActive || u.Email != "ivan.petrov@dila-team.com" || u.Role != domain.RoleBuyer {
		t.Fatalf("после кода: %+v", u)
	}
	if !e.tg.anyTo(stranger, "Почта подтверждена") {
		t.Fatal("нет сообщения об успехе")
	}
	if m, _ := e.tg.lastTo(stranger); !strings.Contains(m.Text, "Какие задания вы готовы выполнять") {
		t.Fatalf("после входа выбор типов заданий: %q", m.Text)
	}
	if !e.tg.anyTo(testAdmin, "Вошёл по почте") || !e.tg.anyTo(testAdmin, "ivan.petrov@dila-team.com") {
		t.Fatal("админ должен получить уведомление о входе")
	}
	if e.tg.deletedCount() <= before {
		t.Fatal("сообщение с кодом должно удаляться из чата")
	}
	// Код одноразовый, а сам пользователь теперь обычный: дальше работают меню и выбор типов.
	e.click(stranger, "kd:ok:1")
	e.say(stranger, "/start")
	if got := e.tg.last(); !strings.Contains(got, "Здравствуйте") {
		t.Fatalf("меню после входа: %q", got)
	}
	// Админ видит почту в карточке.
	e.click(testAdmin, "adm:uc:9000")
	if got := e.tg.last(); !strings.Contains(got, "Почта: ivan.petrov@dila-team.com") {
		t.Fatalf("карточка пользователя: %q", got)
	}
}

// Код привязан ко времени: после 300 секунд не подходит.
func TestEmailLoginExpired(t *testing.T) {
	e, mail := newEmailEnv(t)
	const stranger = int64(9001)
	e.say(stranger, "a@listee-team.com")
	code := mail.code()
	// Сдвигаем срок кода в прошлое, как будто прошло больше 300 секунд.
	if _, err := e.store.DB().Exec(`UPDATE email_otps SET expires_at = expires_at - 400, sent_at = sent_at - 400 WHERE tg_id = ?`, stranger); err != nil {
		t.Fatal(err)
	}
	e.say(stranger, code)
	if u, _ := e.app.access.Lookup(context.Background(), stranger); u != nil {
		t.Fatal("по просроченному коду доступа быть не должно")
	}
	// Код уже не «действующий», бот снова просит почту.
	if got := e.tg.last(); !strings.Contains(got, "рабочую почту") {
		t.Fatalf("после истечения: %q", got)
	}
	// Новый запрос работает.
	e.say(stranger, "a@listee-team.com")
	e.say(stranger, mail.code())
	if u, _ := e.app.access.Lookup(context.Background(), stranger); u == nil || u.Status != domain.StatusActive {
		t.Fatalf("после нового кода доступ должен открыться: %+v", u)
	}
}

// Переключение режима: старые пользователи остаются, регистрация заново не нужна;
// ожидающие заявки и заблокированные не меняются; инвайты не работают.
func TestEmailModeKeepsExistingUsers(t *testing.T) {
	e := newTestEnv(t) // режим по инвайтам
	e.addBuyer(t, 2000, "Алия")
	now := time.Now().UTC()
	for _, u := range []*domain.User{
		{TgID: 2001, Role: domain.RoleBuyer, Status: domain.StatusPending, Lang: "ru", FirstName: "Ожидает", CreatedAt: now, UpdatedAt: now},
		{TgID: 2002, Role: domain.RoleBuyer, Status: domain.StatusBlocked, Lang: "ru", FirstName: "Заблокирован", CreatedAt: now, UpdatedAt: now},
	} {
		if err := e.store.Repos().Users.Create(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	inv, err := e.app.access.CreateInvite(context.Background(), testAdmin, 1, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Включаем вход по почте.
	m := &captureMail{}
	e.app.UseEmailAuth(service.NewEmailAuth(e.store, m, []string{"dila-team.com"}, 300*time.Second))

	e.say(2000, "/start") // активный без почты: пускает как раньше
	if got := e.tg.last(); !strings.Contains(got, "Здравствуйте") {
		t.Fatalf("существующий пользователь: %q", got)
	}
	e.say(2001, "/start")
	if got := e.tg.last(); !strings.Contains(got, "заявка на рассмотрении") {
		t.Fatalf("ожидающая заявка остаётся ожидающей: %q", got)
	}
	e.say(2002, "/start")
	if got := e.tg.last(); !strings.Contains(got, "Нет доступа") {
		t.Fatalf("заблокированный: %q", got)
	}
	// Админ может по-прежнему одобрить старую заявку.
	e.click(testAdmin, "adm:us:2001:a")
	if u, _ := e.app.access.Lookup(context.Background(), 2001); u.Status != domain.StatusActive {
		t.Fatalf("одобрение заявки в режиме почты: %+v", u)
	}
	// Инвайт посторонним больше не открывает доступ: бот просит почту.
	e.say(9100, "/start "+inv.Code)
	if got := e.tg.last(); !strings.Contains(got, "рабочую почту") {
		t.Fatalf("инвайт в режиме почты: %q", got)
	}
	if u, _ := e.app.access.Lookup(context.Background(), 9100); u != nil {
		t.Fatal("по инвайту в режиме почты регистрироваться нельзя")
	}
	// Обычные кнопки незнакомца по-прежнему отклоняются.
	e.click(9100, "tsk:l")
	if got := e.tg.lastAnswer(); got != "Нет доступа." {
		t.Fatalf("кнопка незнакомца: %q", got)
	}
	// Админу в экране инвайтов показано предупреждение.
	e.click(testAdmin, "adm:inv")
	if got := e.tg.last(); !strings.Contains(got, "AUTH_MODE=email") {
		t.Fatalf("предупреждение в экране инвайтов: %q", got)
	}

	// Обратно на инвайты: сотрудники, вошедшие по почте, остаются.
	e.say(9200, "a@dila-team.com")
	e.say(9200, m.code())
	if u, _ := e.app.access.Lookup(context.Background(), 9200); u == nil || u.Email == "" {
		t.Fatal("вход по почте не сработал")
	}
	e.app.authMode = "invite"
	e.say(9200, "/start")
	if got := e.tg.last(); !strings.Contains(got, "Здравствуйте") && !strings.Contains(got, "Какие задания") {
		t.Fatalf("после возврата на инвайты пользователь по почте остаётся: %q", got)
	}
}

// Вход по почте: после ввода пять раз неверного кода он аннулируется.
func TestEmailLoginAttemptsLimit(t *testing.T) {
	e, mail := newEmailEnv(t)
	const stranger = int64(9002)
	e.say(stranger, "a@dila-team.com")
	code := mail.code()
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	for i := 0; i < service.MaxOTPAttempts; i++ {
		e.say(stranger, wrong)
	}
	if got := e.tg.last(); !strings.Contains(got, "аннулирован") {
		t.Fatalf("пятая неверная попытка: %q", got)
	}
	e.say(stranger, code)
	if u, _ := e.app.access.Lookup(context.Background(), stranger); u != nil {
		t.Fatal("после сгорания кода верный код доступа не даёт")
	}
}
