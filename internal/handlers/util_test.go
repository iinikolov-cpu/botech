package handlers

import (
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	now := time.Now()
	l := newLimiter(2, time.Minute)
	l.now = func() time.Time { return now }

	steps := []struct {
		name    string
		advance time.Duration
		want    bool
	}{
		{"первое событие", 0, true},
		{"второе событие", 10 * time.Second, true},
		{"третье в окне блокируется", 10 * time.Second, false},
		{"после окна снова можно", 61 * time.Second, true},
	}
	for _, s := range steps {
		now = now.Add(s.advance)
		if got := l.Allow(1); got != s.want {
			t.Fatalf("%s: Allow=%v, ожидали %v", s.name, got, s.want)
		}
	}
	if !l.Allow(2) {
		t.Fatal("лимит должен быть отдельным для каждого ключа")
	}
}

func TestMaskID(t *testing.T) {
	tests := map[int64]string{123456789: "***6789", 12: "***"}
	for in, want := range tests {
		if got := maskID(in); got != want {
			t.Errorf("maskID(%d)=%s, ожидали %s", in, got, want)
		}
	}
}
