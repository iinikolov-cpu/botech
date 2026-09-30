package domain

import "time"

// TaskStatus статус задания.
type TaskStatus string

const (
	TaskCreated   TaskStatus = "created"   // создано, ещё не доставлено покупателю
	TaskSent      TaskStatus = "sent"      // отправлено, ждёт ответа
	TaskAccepted  TaskStatus = "accepted"  // принято, идёт отсчёт срока
	TaskDeclined  TaskStatus = "declined"  // покупатель отказался
	TaskReported  TaskStatus = "reported"  // отчёт получен
	TaskExpired   TaskStatus = "expired"   // срок вышел без отчёта
	TaskReviewed  TaskStatus = "reviewed"  // проверено админом
	TaskCancelled TaskStatus = "cancelled" // отменено админом
)

// transitions единственный источник правды о допустимых сменах статуса.
var transitions = map[TaskStatus][]TaskStatus{
	TaskCreated:  {TaskSent},
	TaskSent:     {TaskAccepted, TaskDeclined},
	TaskAccepted: {TaskReported, TaskExpired, TaskCancelled},
	TaskExpired:  {TaskReported, TaskCancelled}, // опоздавший отчёт всё равно принимаем
	TaskReported: {TaskReviewed},
}

// CanTransition можно ли перейти из from в to.
func CanTransition(from, to TaskStatus) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// IsActive задание не завершено (блокирует повторную выдачу того же сценария).
func (s TaskStatus) IsActive() bool {
	switch s {
	case TaskCreated, TaskSent, TaskAccepted, TaskExpired:
		return true
	}
	return false
}

var taskStatusTitles = map[TaskStatus]string{
	TaskCreated:   "создано",
	TaskSent:      "отправлено",
	TaskAccepted:  "принято",
	TaskDeclined:  "отказ",
	TaskReported:  "отчёт получен",
	TaskExpired:   "просрочено",
	TaskReviewed:  "проверено",
	TaskCancelled: "отменено",
}

// Title русское название статуса.
func (s TaskStatus) Title() string {
	if t, ok := taskStatusTitles[s]; ok {
		return t
	}
	return string(s)
}

// Task задание, выданное покупателю по конкретной версии сценария.
type Task struct {
	ID                int64
	ScenarioID        int64
	ScenarioVersionID int64
	UserID            int64
	Status            TaskStatus
	DueDays           int // срок в днях, отсчитывается от принятия
	CreatedBy         int64
	CreatedAt         time.Time
	SentAt            time.Time
	AcceptedAt        time.Time
	DueAt             time.Time
	DeclinedAt        time.Time
	ReportedAt        time.Time
	ReviewedAt        time.Time
}

// TaskEvent запись истории задания.
type TaskEvent struct {
	ID         int64
	TaskID     int64
	Kind       string // created, sent, send_failed, status
	FromStatus TaskStatus
	ToStatus   TaskStatus
	ActorID    int64
	Details    string
	At         time.Time
}
