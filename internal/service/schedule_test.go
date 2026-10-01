package service

import (
	"reflect"
	"testing"
	"time"
)

func TestNextStep(t *testing.T) {
	base := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	h := func(n int) time.Time { return base.Add(time.Duration(n) * time.Hour) }
	offsets := HoursToDurations([]int{24, 48})
	esc := 24 * time.Hour

	tests := []struct {
		name string
		now  time.Time
		done map[int]bool
		want Step
	}{
		{"до первого напоминания", h(23), nil, Step{}},
		{"ровно в момент первого", h(24), nil, Step{Remind: 1}},
		{"первое уже отправлено", h(25), map[int]bool{1: true}, Step{}},
		{"второе напоминание", h(48), map[int]bool{1: true}, Step{Remind: 2}},
		{"между вторым и эскалацией", h(60), map[int]bool{1: true, 2: true}, Step{}},
		{"эскалация через сутки после последнего", h(72), map[int]bool{1: true, 2: true}, Step{Escalate: true}},
		{"эскалация уже была", h(100), map[int]bool{0: true, 1: true, 2: true}, Step{}},
		{"бот долго не работал: только последнее, первое пропущено", h(60), nil, Step{Remind: 2, Skipped: []int{1}}},
		{"после пропуска ничего лишнего", h(61), map[int]bool{1: true, 2: true}, Step{}},
		{"эскалации нет, пока не отправлено последнее напоминание", h(80), map[int]bool{1: true}, Step{Remind: 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NextStep(base, tc.now, offsets, esc, tc.done); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("получили %+v, ожидали %+v", got, tc.want)
			}
		})
	}

	t.Run("нет интервалов", func(t *testing.T) {
		if got := NextStep(base, h(500), nil, esc, nil); !reflect.DeepEqual(got, Step{}) {
			t.Fatalf("без интервалов ничего не делаем: %+v", got)
		}
	})
	t.Run("нулевой базовый момент", func(t *testing.T) {
		if got := NextStep(time.Time{}, h(500), offsets, esc, nil); !reflect.DeepEqual(got, Step{}) {
			t.Fatalf("нет базы: %+v", got)
		}
	})
	t.Run("один интервал и эскалация", func(t *testing.T) {
		one := HoursToDurations([]int{10})
		if got := NextStep(base, h(10), one, esc, nil); got.Remind != 1 {
			t.Fatalf("%+v", got)
		}
		if got := NextStep(base, h(34), one, esc, map[int]bool{1: true}); !got.Escalate {
			t.Fatalf("%+v", got)
		}
	})
}

func TestParseHours(t *testing.T) {
	tests := []struct {
		in      string
		want    []int
		wantErr bool
	}{
		{"24,48", []int{24, 48}, false},
		{" 24; 48 ", []int{24, 48}, false},
		{"12 24 72", []int{12, 24, 72}, false},
		{"off", nil, false},
		{"", nil, false},
		{"48,24", nil, true},       // не по возрастанию
		{"24,24", nil, true},       // повтор
		{"0,5", nil, true},         // ноль
		{"abc", nil, true},         // не число
		{"721", nil, true},         // слишком много
		{"1,2,3,4,5,6", nil, true}, // больше лимита
	}
	for _, tc := range tests {
		got, err := ParseHours(tc.in)
		if (err != nil) != tc.wantErr || (!tc.wantErr && !reflect.DeepEqual(got, tc.want)) {
			t.Errorf("ParseHours(%q) = %v, %v; ожидали %v (ошибка=%v)", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestParseQuietAndClock(t *testing.T) {
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
	if on, h, m, err := ParseClock("03:30"); err != nil || !on || h != 3 || m != 30 {
		t.Errorf("03:30: %v %d %d %v", on, h, m, err)
	}
	for _, bad := range []string{"3", "24:00", "10:60", "xx:yy"} {
		if _, _, _, err := ParseClock(bad); err == nil {
			t.Errorf("ParseClock(%q) должен давать ошибку", bad)
		}
	}
}

func TestInQuiet(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 10, 5, h, 30, 0, 0, time.UTC) }
	tests := []struct {
		from, to, hour int
		want           bool
	}{
		{22, 9, 23, true}, {22, 9, 3, true}, {22, 9, 9, false}, {22, 9, 21, false}, {22, 9, 22, true},
		{1, 6, 3, true}, {1, 6, 6, false}, {1, 6, 0, false}, // окно внутри суток
	}
	for _, tc := range tests {
		if got := InQuiet(at(tc.hour), tc.from, tc.to); got != tc.want {
			t.Errorf("InQuiet(%d:30, %d-%d) = %v, ожидали %v", tc.hour, tc.from, tc.to, got, tc.want)
		}
	}
}
