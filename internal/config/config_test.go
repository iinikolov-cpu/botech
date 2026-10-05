package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseMinutes(t *testing.T) {
	tests := []struct {
		in      string
		want    []int
		wantErr bool
	}{
		{"1440,2880", []int{1440, 2880}, false},
		{" 30 ; 60 120 ", []int{30, 60, 120}, false},
		{"5", []int{5}, false},
		{"off", nil, false},
		{"", nil, false},
		{"0", nil, false},
		{"60,30", nil, true},       // не по возрастанию
		{"60,60", nil, true},       // повтор
		{"abc", nil, true},         // не число
		{"-5", nil, true},          // отрицательное
		{"43201", nil, true},       // больше 30 суток
		{"1,2,3,4,5,6", nil, true}, // больше пяти
	}
	for _, tc := range tests {
		got, err := ParseMinutes(tc.in)
		if (err != nil) != tc.wantErr || (!tc.wantErr && !reflect.DeepEqual(got, tc.want)) {
			t.Errorf("ParseMinutes(%q) = %v, %v; ожидали %v (ошибка=%v)", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestParseQuiet(t *testing.T) {
	if on, f, to, err := ParseQuiet("22-9"); err != nil || !on || f != 22 || to != 9 {
		t.Errorf("22-9: %v %d %d %v", on, f, to, err)
	}
	if on, _, _, err := ParseQuiet("off"); err != nil || on {
		t.Errorf("off: %v %v", on, err)
	}
	for _, bad := range []string{"9", "25-3", "5-5", "a-b", "-1-3"} {
		if _, _, _, err := ParseQuiet(bad); err == nil {
			t.Errorf("ParseQuiet(%q) должен давать ошибку", bad)
		}
	}
}

func TestLoadReminderDefaults(t *testing.T) {
	t.Setenv("BOT_TOKEN", "1:x")
	t.Setenv("FIRST_ADMIN_ID", "5")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.RemindAcceptMinutes, []int{1440, 2880}) || !reflect.DeepEqual(c.RemindReportMinutes, []int{1440, 2880}) ||
		!c.QuietOn || c.QuietFrom != 22 || c.QuietTo != 9 || c.StaleMinutes != 1440 {
		t.Fatalf("значения по умолчанию: %+v", c)
	}
	t.Setenv("REMIND_ACCEPT_MINUTES", "10,20")
	t.Setenv("REMIND_REPORT_MINUTES", "off")
	t.Setenv("REMIND_QUIET_HOURS", "off")
	t.Setenv("STALE_MINUTES", "90")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.RemindAcceptMinutes, []int{10, 20}) || len(c.RemindReportMinutes) != 0 || c.QuietOn || c.StaleMinutes != 90 {
		t.Fatalf("свои значения: %+v", c)
	}
	t.Setenv("REMIND_ACCEPT_MINUTES", "20,10")
	if _, err := Load(); err == nil {
		t.Fatal("неверный порядок должен давать ошибку")
	}
	t.Setenv("REMIND_ACCEPT_MINUTES", "10")
	t.Setenv("STALE_MINUTES", "0")
	if _, err := Load(); err == nil {
		t.Fatal("STALE_MINUTES=0 должен давать ошибку")
	}
}

func TestLoadPromoMaxUses(t *testing.T) {
	t.Setenv("BOT_TOKEN", "1:x")
	t.Setenv("FIRST_ADMIN_ID", "5")
	c, err := Load()
	if err != nil || c.PromoMaxUses != 1 {
		t.Fatalf("по умолчанию: %v %+v", err, c)
	}
	t.Setenv("PROMO_LOW_THRESHOLD", "4") // старое имя продолжает работать
	if c, err = Load(); err != nil || c.PromoMaxUses != 4 {
		t.Fatalf("старое имя: %v %+v", err, c)
	}
	t.Setenv("PROMO_MAX_USES", "7") // новое имя главнее
	if c, err = Load(); err != nil || c.PromoMaxUses != 7 {
		t.Fatalf("новое имя: %v %+v", err, c)
	}
	t.Setenv("PROMO_MAX_USES", "0")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "PROMO_MAX_USES") {
		t.Fatalf("ноль должен давать ошибку с именем переменной: %v", err)
	}
}
