package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"botech/internal/domain"
)

// fakeMail запоминает отправленные письма и умеет «ломаться».
type fakeMail struct {
	mu   sync.Mutex
	sent []sentMail
	fail error
}

type sentMail struct{ to, subject, body string }

func (f *fakeMail) Send(_ context.Context, to, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.sent = append(f.sent, sentMail{to, subject, body})
	return nil
}

func (f *fakeMail) last() sentMail {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return sentMail{}
	}
	return f.sent[len(f.sent)-1]
}

func (f *fakeMail) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

var testDomains = []string{"dila-team.com", "listee-team.com"}

// codeOf достаёт код из последнего письма.
func codeOf(t *testing.T, m *fakeMail) string {
	t.Helper()
	for _, f := range strings.Fields(m.last().body) {
		if len(f) == OTPLength && strings.Trim(f, "0123456789") == "" {
			return f
		}
	}
	t.Fatalf("в письме нет кода: %q", m.last().body)
	return ""
}

type authEnv struct {
	*env
	auth *EmailAuth
	mail *fakeMail
	clk  *clock
}

func newAuthEnv(t *testing.T) *authEnv {
	t.Helper()
	e := newEnv(t)
	m := &fakeMail{}
	c := &clock{t: t0}
	a := NewEmailAuth(e.store, m, testDomains, 300*time.Second)
	a.now = c.Now
	return &authEnv{env: e, auth: a, mail: m, clk: c}
}

func TestNormalizeEmail(t *testing.T) {
	tests := []struct {
		in        string
		canonical string
		deliver   string
		err       bool
	}{
		{"Ivan.Petrov@Dila-Team.com", "ivan.petrov@dila-team.com", "ivan.petrov@dila-team.com", false},
		{"  a@listee-team.com ", "a@listee-team.com", "a@listee-team.com", false},
		{"<a@listee-team.com>", "a@listee-team.com", "a@listee-team.com", false},
		{"a+test@dila-team.com", "a@dila-team.com", "a+test@dila-team.com", false}, // метка срезается для уникальности
		{"o'neil@dila-team.com", "o'neil@dila-team.com", "o'neil@dila-team.com", false},
		{"a@gmail.com", "", "", true},              // чужой домен
		{"a@sub.dila-team.com", "", "", true},      // поддомен не разрешён
		{"a@dila-team.com.evil.com", "", "", true}, // домен только полным совпадением
		{"a@evil.com@dila-team.com", "", "", true},
		{"a@@dila-team.com", "", "", true},
		{"@dila-team.com", "", "", true},
		{"dila-team.com", "", "", true},
		{"", "", "", true},
		{"a b@dila-team.com", "", "", true},
		{"a,b@dila-team.com", "", "", true},
		{"a..b@dila-team.com", "", "", true},
		{".a@dila-team.com", "", "", true},
		{"a\r\nBcc: x@evil.com@dila-team.com", "", "", true}, // подмешивание заголовков
		{strings.Repeat("a", 65) + "@dila-team.com", "", "", true},
	}
	for _, tc := range tests {
		c, d, err := NormalizeEmail(tc.in, testDomains)
		if (err != nil) != tc.err || c != tc.canonical || d != tc.deliver {
			t.Errorf("NormalizeEmail(%q) = %q, %q, %v; ожидали %q, %q, ошибка=%v", tc.in, c, d, err, tc.canonical, tc.deliver, tc.err)
		}
		if tc.err && !errors.Is(err, ErrBadEmail) {
			t.Errorf("NormalizeEmail(%q): ошибка должна быть ErrBadEmail: %v", tc.in, err)
		}
	}
}

func TestMaskEmail(t *testing.T) {
	if got := MaskEmail("ivan.petrov@dila-team.com"); got != "iv***@dila-team.com" {
		t.Fatalf("маска: %q", got)
	}
	if got := MaskEmail("a@dila-team.com"); got != "a***@dila-team.com" || strings.Contains(got, "a@") {
		t.Fatalf("маска короткой: %q", got)
	}
}

func TestRandomCode(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c, err := randomCode()
		if err != nil || len(c) != OTPLength || strings.Trim(c, "0123456789") != "" {
			t.Fatalf("код %q: %v", c, err)
		}
		seen[c] = true
	}
	if len(seen) < 150 {
		t.Fatalf("коды не похожи на случайные: %d уникальных из 200", len(seen))
	}
}

// Основной путь: почта, письмо с кодом, ввод кода, доступ выдан.
func TestEmailAuthHappyPath(t *testing.T) {
	ctx := context.Background()
	e := newAuthEnv(t)
	p := Profile{TgID: 500, FirstName: "Иван", Username: "ivan"}

	email, err := e.auth.RequestCode(ctx, p, "Ivan@Dila-Team.com")
	if err != nil || email != "ivan@dila-team.com" {
		t.Fatalf("запрос кода: %q %v", email, err)
	}
	if e.mail.count() != 1 || e.mail.last().to != "ivan@dila-team.com" || !strings.Contains(e.mail.last().body, "5 мин") {
		t.Fatalf("письмо: %+v", e.mail.last())
	}
	if !e.auth.HasPending(ctx, 500) {
		t.Fatal("код должен быть действующим")
	}
	// Код не хранится в открытом виде.
	var stored string
	if err := e.store.DB().QueryRow(`SELECT code_hash FROM email_otps WHERE tg_id = 500`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	code := codeOf(t, e.mail)
	if strings.Contains(stored, code) || len(stored) != 64 {
		t.Fatalf("в базе должен лежать хэш, а не код: %q", stored)
	}

	// До ввода кода пользователя нет.
	if u, _ := e.access.Lookup(ctx, 500); u != nil {
		t.Fatal("до подтверждения доступа быть не должно")
	}
	u, err := e.auth.VerifyCode(ctx, p, code)
	if err != nil || u.Status != domain.StatusActive || u.Role != domain.RoleBuyer || u.Email != "ivan@dila-team.com" || len(u.Kinds) != 0 {
		t.Fatalf("после кода: %+v %v", u, err)
	}
	if got, _ := e.access.Lookup(ctx, 500); got == nil || got.Email != "ivan@dila-team.com" || got.Status != domain.StatusActive || got.IsAdmin() {
		t.Fatalf("пользователь не сохранён: %+v", got)
	}
	// Код одноразовый.
	if _, err := e.auth.VerifyCode(ctx, p, code); !errors.Is(err, ErrNoPendingCode) {
		t.Fatalf("повторное использование кода: %v", err)
	}
	// Вход записан в журнал без полной почты.
	entries, _ := e.store.Repos().Audit.List(ctx, 10)
	found := false
	for _, a := range entries {
		if a.Action == "auth.email" && a.EntityID == "500" && strings.Contains(a.Details, "iv***@") && !strings.Contains(a.Details, "ivan@") {
			found = true
		}
	}
	if !found {
		t.Fatalf("журнал: %+v", entries)
	}
}

func TestEmailAuthWrongDomainSendsNothing(t *testing.T) {
	e := newAuthEnv(t)
	for _, bad := range []string{"a@gmail.com", "нет почты", "a@dila-team.com.evil.com"} {
		if _, err := e.auth.RequestCode(context.Background(), Profile{TgID: 501}, bad); !errors.Is(err, ErrBadEmail) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if e.mail.count() != 0 || e.auth.HasPending(context.Background(), 501) {
		t.Fatal("для неподходящей почты письмо не отправляется и код не создаётся")
	}
}

// Срок кода 300 секунд: до этого момента работает, после нет.
func TestEmailAuthExpiry(t *testing.T) {
	ctx := context.Background()
	e := newAuthEnv(t)
	p := Profile{TgID: 502}
	if _, err := e.auth.RequestCode(ctx, p, "a@dila-team.com"); err != nil {
		t.Fatal(err)
	}
	code := codeOf(t, e.mail)
	e.clk.Add(299 * time.Second)
	if !e.auth.HasPending(ctx, 502) {
		t.Fatal("на 299-й секунде код ещё действует")
	}
	e.clk.Add(2 * time.Second) // 301 секунда
	if e.auth.HasPending(ctx, 502) {
		t.Fatal("после 300 секунд код недействителен")
	}
	if _, err := e.auth.VerifyCode(ctx, p, code); !errors.Is(err, ErrCodeExpired) {
		t.Fatalf("просроченный код: %v", err)
	}
	if u, _ := e.access.Lookup(ctx, 502); u != nil {
		t.Fatal("по просроченному коду доступ выдаваться не должен")
	}
	// Верный код ровно в пределах срока проходит.
	if _, err := e.auth.RequestCode(ctx, p, "a@dila-team.com"); err != nil {
		t.Fatal(err)
	}
	e.clk.Add(299 * time.Second)
	if _, err := e.auth.VerifyCode(ctx, p, codeOf(t, e.mail)); err != nil {
		t.Fatalf("код в пределах срока: %v", err)
	}
}

// Пять неверных вводов сжигают код, даже правильный после этого не подходит.
func TestEmailAuthAttempts(t *testing.T) {
	ctx := context.Background()
	e := newAuthEnv(t)
	p := Profile{TgID: 503}
	if _, err := e.auth.RequestCode(ctx, p, "a@dila-team.com"); err != nil {
		t.Fatal(err)
	}
	code := codeOf(t, e.mail)
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	for i := 1; i < MaxOTPAttempts; i++ {
		_, err := e.auth.VerifyCode(ctx, p, wrong)
		if !errors.Is(err, ErrCodeInvalid) || !strings.Contains(err.Error(), fmt.Sprintf("осталось попыток: %d", MaxOTPAttempts-i)) {
			t.Fatalf("попытка %d: %v", i, err)
		}
	}
	if _, err := e.auth.VerifyCode(ctx, p, wrong); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("последняя попытка: %v", err)
	}
	if _, err := e.auth.VerifyCode(ctx, p, code); !errors.Is(err, ErrNoPendingCode) {
		t.Fatalf("после сгорания верный код не должен работать: %v", err)
	}
}

// Код привязан к аккаунту: с другого аккаунта он не подходит.
func TestEmailAuthCodeBoundToAccount(t *testing.T) {
	ctx := context.Background()
	e := newAuthEnv(t)
	if _, err := e.auth.RequestCode(ctx, Profile{TgID: 504}, "a@dila-team.com"); err != nil {
		t.Fatal(err)
	}
	code := codeOf(t, e.mail)
	if _, err := e.auth.VerifyCode(ctx, Profile{TgID: 505}, code); !errors.Is(err, ErrNoPendingCode) {
		t.Fatalf("чужой аккаунт: %v", err)
	}
}

func TestEmailAuthRateLimits(t *testing.T) {
	ctx := context.Background()
	e := newAuthEnv(t)
	p := Profile{TgID: 506}

	if _, err := e.auth.RequestCode(ctx, p, "a@dila-team.com"); err != nil {
		t.Fatal(err)
	}
	// Повторно сразу нельзя: пауза.
	var tm *TooManyRequestsError
	if _, err := e.auth.RequestCode(ctx, p, "a@dila-team.com"); !errors.As(err, &tm) || tm.Wait <= 0 || tm.Wait > ResendCooldown {
		t.Fatalf("пауза между отправками: %v", err)
	}
	// Новый код после паузы заменяет прежний: старый больше не подходит.
	first := codeOf(t, e.mail)
	e.clk.Add(ResendCooldown + time.Second)
	if _, err := e.auth.RequestCode(ctx, p, "a@dila-team.com"); err != nil {
		t.Fatal(err)
	}
	second := codeOf(t, e.mail)
	if first != second {
		if _, err := e.auth.VerifyCode(ctx, p, first); !errors.Is(err, ErrCodeInvalid) {
			t.Fatalf("прежний код после нового запроса: %v", err)
		}
	}
	// Не больше MaxSendsPerTg писем за окно с одного аккаунта.
	for i := 3; i <= MaxSendsPerTg; i++ {
		e.clk.Add(ResendCooldown + time.Second)
		if _, err := e.auth.RequestCode(ctx, p, fmt.Sprintf("u%d@dila-team.com", i)); err != nil {
			t.Fatalf("отправка %d: %v", i, err)
		}
	}
	e.clk.Add(ResendCooldown + time.Second)
	if _, err := e.auth.RequestCode(ctx, p, "z@dila-team.com"); !errors.As(err, &tm) || tm.Wait < time.Minute {
		t.Fatalf("лимит на аккаунт: %v", err)
	}
	// Через час окно очищается.
	e.clk.Add(SendWindow)
	if _, err := e.auth.RequestCode(ctx, p, "z@dila-team.com"); err != nil {
		t.Fatalf("после окна: %v", err)
	}

	// Лимит на одну почту: разные аккаунты не могут завалить один ящик.
	e2 := newAuthEnv(t)
	for i := 0; i < MaxSendsPerEmail; i++ {
		if _, err := e2.auth.RequestCode(ctx, Profile{TgID: int64(600 + i)}, "victim@dila-team.com"); err != nil {
			t.Fatalf("отправка %d: %v", i, err)
		}
	}
	if _, err := e2.auth.RequestCode(ctx, Profile{TgID: 700}, "victim@dila-team.com"); !errors.As(err, &tm) {
		t.Fatalf("лимит на почту: %v", err)
	}
	// Метка +tag не позволяет обойти лимит: это та же почта.
	if _, err := e2.auth.RequestCode(ctx, Profile{TgID: 701}, "victim+1@dila-team.com"); !errors.As(err, &tm) {
		t.Fatalf("обход лимита через +метку: %v", err)
	}
}

func TestEmailAuthMailFailureRevokesCode(t *testing.T) {
	ctx := context.Background()
	e := newAuthEnv(t)
	e.mail.fail = errors.New("smtp: пароль неверный")
	_, err := e.auth.RequestCode(ctx, Profile{TgID: 507}, "a@dila-team.com")
	if !errors.Is(err, ErrMailFailed) {
		t.Fatalf("сбой почты: %v", err)
	}
	if e.auth.HasPending(ctx, 507) {
		t.Fatal("недоставленный код должен быть отозван")
	}
}

// Одна почта: один аккаунт. Освобождается, когда прежний владелец заблокирован.
func TestEmailAuthUniqueEmail(t *testing.T) {
	ctx := context.Background()
	e := newAuthEnv(t)
	login := func(tg int64, email string) (*domain.User, error) {
		p := Profile{TgID: tg}
		if _, err := e.auth.RequestCode(ctx, p, email); err != nil {
			return nil, err
		}
		return e.auth.VerifyCode(ctx, p, codeOf(t, e.mail))
	}
	if _, err := login(510, "emp@dila-team.com"); err != nil {
		t.Fatal(err)
	}
	// Тот же адрес с другого аккаунта, в том числе с +меткой: нельзя.
	for _, em := range []string{"emp@dila-team.com", "EMP+x@dila-team.com"} {
		if _, err := e.auth.RequestCode(ctx, Profile{TgID: 511}, em); !errors.Is(err, ErrEmailTaken) {
			t.Fatalf("%s: %v", em, err)
		}
	}
	if e.auth.HasPending(ctx, 511) {
		t.Fatal("для занятой почты код не создаётся")
	}
	// Админ заблокировал старый аккаунт: сотрудник входит с нового.
	if _, err := e.access.SetStatus(ctx, firstAdmin, 510, domain.StatusBlocked); err != nil {
		t.Fatal(err)
	}
	if u, err := login(511, "emp@dila-team.com"); err != nil || u.Email != "emp@dila-team.com" {
		t.Fatalf("после блокировки владельца: %v %v", u, err)
	}
	old, _ := e.access.Lookup(ctx, 510)
	if old.Email != "" || old.Status != domain.StatusBlocked {
		t.Fatalf("у заблокированного почта снимается, он остаётся заблокированным: %+v", old)
	}
}

// Уже зарегистрированного (например, по инвайту) эта цепочка не трогает: он не проходит повторную регистрацию.
func TestEmailAuthExistingUserKept(t *testing.T) {
	ctx := context.Background()
	e := newAuthEnv(t)
	// Пользователь 1 создан в newEnv как активный без почты. Попытка выдать ему код не должна менять данные.
	before, _ := e.access.Lookup(ctx, 1)
	p := Profile{TgID: 1}
	if _, err := e.auth.RequestCode(ctx, p, "x@dila-team.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.auth.VerifyCode(ctx, p, codeOf(t, e.mail)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("существующий пользователь не регистрируется заново: %v", err)
	}
	after, _ := e.access.Lookup(ctx, 1)
	if after.Status != before.Status || after.Email != "" || !after.CanDo(domain.KindBuyer) {
		t.Fatalf("данные пользователя изменились: %+v -> %+v", before, after)
	}
}
