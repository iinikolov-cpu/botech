package service

import "time"

// Step решение планировщика по одному заданию на текущий момент.
type Step struct {
	Remind  int   // номер напоминания покупателю, которое нужно отправить сейчас (0 = нечего отправлять)
	Skipped []int // более ранние напоминания, которые уже просрочены и отдельно не отправляются
}

// NextStep чистая функция расчёта напоминаний (без обращений к БД, поэтому легко проверяется).
//
//   - offsets: через какие промежутки после base напоминать (по возрастанию).
//   - done: номера уже записанных в журнале напоминаний (1..N).
//
// Если бот долго не работал и просрочено сразу несколько напоминаний, отправляется только
// последнее из них, а предыдущие помечаются пропущенными: покупатель не получает пачку сообщений.
func NextStep(base, now time.Time, offsets []time.Duration, done map[int]bool) Step {
	if len(offsets) == 0 || base.IsZero() {
		return Step{}
	}
	last := 0 // последнее наступившее напоминание
	for i, off := range offsets {
		if !now.Before(base.Add(off)) {
			last = i + 1
		}
	}
	if last == 0 || done[last] {
		return Step{}
	}
	var skipped []int
	for i := 1; i < last; i++ {
		if !done[i] {
			skipped = append(skipped, i)
		}
	}
	return Step{Remind: last, Skipped: skipped}
}

// MinutesToDurations переводит минуты в промежутки времени.
func MinutesToDurations(m []int) []time.Duration {
	out := make([]time.Duration, len(m))
	for i, n := range m {
		out[i] = time.Duration(n) * time.Minute
	}
	return out
}
