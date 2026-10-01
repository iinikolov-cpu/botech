package service

import "time"

// Step решение планировщика по одному заданию на текущий момент.
type Step struct {
	Remind   int   // номер напоминания покупателю, которое нужно отправить сейчас (0 = нечего отправлять)
	Skipped  []int // более ранние напоминания, которые уже просрочены и отдельно не отправляются
	Escalate bool  // пора сообщить админу
}

// NextStep чистая функция расчёта напоминаний (без обращений к БД, поэтому легко проверяется).
//
//   - offsets: через какие промежутки после base напоминать (по возрастанию).
//   - escalate: через сколько после последнего напоминания звать админа.
//   - done: что уже записано в журнале: номера 1..N напоминаний и 0 для эскалации.
//
// Если бот долго не работал и просрочено сразу несколько напоминаний, отправляется только
// последнее из них, а предыдущие помечаются пропущенными: покупатель не получает пачку сообщений.
func NextStep(base, now time.Time, offsets []time.Duration, escalate time.Duration, done map[int]bool) Step {
	n := len(offsets)
	if n == 0 || base.IsZero() {
		return Step{}
	}
	// Последнее наступившее напоминание.
	last := 0
	for i := 0; i < n; i++ {
		if !now.Before(base.Add(offsets[i])) {
			last = i + 1
		}
	}
	if last > 0 && !done[last] {
		var skipped []int
		for i := 1; i < last; i++ {
			if !done[i] {
				skipped = append(skipped, i)
			}
		}
		return Step{Remind: last, Skipped: skipped}
	}
	// Эскалация: все напоминания пройдены, а покупатель всё ещё молчит.
	if last == n && done[n] && !done[0] && escalate > 0 && !now.Before(base.Add(offsets[n-1]).Add(escalate)) {
		return Step{Escalate: true}
	}
	return Step{}
}

// HoursToDurations переводит часы в промежутки времени.
func HoursToDurations(h []int) []time.Duration {
	out := make([]time.Duration, len(h))
	for i, n := range h {
		out[i] = time.Duration(n) * time.Hour
	}
	return out
}
