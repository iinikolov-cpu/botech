package service

import (
	"reflect"
	"testing"
	"time"
)

func TestNextStep(t *testing.T) {
	base := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	h := func(n int) time.Time { return base.Add(time.Duration(n) * time.Hour) }
	offsets := MinutesToDurations([]int{24 * 60, 48 * 60})

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
		{"после последнего тишина", h(500), map[int]bool{1: true, 2: true}, Step{}},
		{"бот долго не работал: только последнее, первое пропущено", h(60), nil, Step{Remind: 2, Skipped: []int{1}}},
		{"после пропуска ничего лишнего", h(61), map[int]bool{1: true, 2: true}, Step{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NextStep(base, tc.now, offsets, tc.done); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("получили %+v, ожидали %+v", got, tc.want)
			}
		})
	}

	t.Run("нет интервалов", func(t *testing.T) {
		if got := NextStep(base, h(500), nil, nil); !reflect.DeepEqual(got, Step{}) {
			t.Fatalf("без интервалов ничего не делаем: %+v", got)
		}
	})
	t.Run("нулевой базовый момент", func(t *testing.T) {
		if got := NextStep(time.Time{}, h(500), offsets, nil); !reflect.DeepEqual(got, Step{}) {
			t.Fatalf("нет базы: %+v", got)
		}
	})
	t.Run("интервалы в минутах", func(t *testing.T) {
		m := MinutesToDurations([]int{5, 10})
		if got := NextStep(base, base.Add(5*time.Minute), m, nil); got.Remind != 1 {
			t.Fatalf("через 5 минут: %+v", got)
		}
		if got := NextStep(base, base.Add(4*time.Minute), m, nil); got.Remind != 0 {
			t.Fatalf("через 4 минуты рано: %+v", got)
		}
	})
}

func TestParseClock(t *testing.T) {
	if on, h, m, err := ParseClock("03:30"); err != nil || !on || h != 3 || m != 30 {
		t.Errorf("03:30: %v %d %d %v", on, h, m, err)
	}
	if on, _, _, err := ParseClock("off"); err != nil || on {
		t.Errorf("off: %v %v", on, err)
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
