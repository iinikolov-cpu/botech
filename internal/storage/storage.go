// Пакет storage описывает интерфейсы репозиториев. Бизнес-логика зависит
// только от них, поэтому переход с SQLite на PostgreSQL не затронет service.
package storage

import (
	"context"
	"errors"
	"time"

	"botech/internal/domain"
)

// ErrNotFound возвращается, когда запись не найдена.
var ErrNotFound = errors.New("не найдено")

// ErrDuplicate возвращается при нарушении уникальности (например, второе активное задание).
var ErrDuplicate = errors.New("уже существует")

// UserRepo работа с пользователями.
type UserRepo interface {
	Get(ctx context.Context, tgID int64) (*domain.User, error) // ErrNotFound, если нет
	Create(ctx context.Context, u *domain.User) error
	Update(ctx context.Context, u *domain.User) error
	List(ctx context.Context, role domain.Role, status domain.UserStatus, limit, offset int) ([]*domain.User, error)
	Count(ctx context.Context, role domain.Role, status domain.UserStatus) (int, error)
	ListActiveAdmins(ctx context.Context) ([]*domain.User, error)
	// SetKinds записывает типы заданий, которые пользователь готов делать.
	SetKinds(ctx context.Context, tgID int64, kinds []domain.TaskKind, at time.Time) error
	// ListForKind и CountForKind: пользователи роли buyer с данным статусом, готовые делать задания этого типа.
	ListForKind(ctx context.Context, status domain.UserStatus, kind domain.TaskKind, limit, offset int) ([]*domain.User, error)
	CountForKind(ctx context.Context, status domain.UserStatus, kind domain.TaskKind) (int, error)
}

// InviteRepo работа с инвайтами.
type InviteRepo interface {
	Create(ctx context.Context, inv *domain.Invite) error
	Get(ctx context.Context, code string) (*domain.Invite, error)
	// Redeem атомарно увеличивает счётчик использований. false, если код
	// недействителен (нет, отозван, просрочен, лимит исчерпан).
	Redeem(ctx context.Context, code string) (bool, error)
	Revoke(ctx context.Context, code string) error
	ListActive(ctx context.Context, limit int) ([]*domain.Invite, error)
}

// AuditRepo журнал действий админов.
type AuditRepo interface {
	Add(ctx context.Context, e *domain.AuditEntry) error
	List(ctx context.Context, limit int) ([]*domain.AuditEntry, error)
}

// ScenarioRepo сценарии и их версии.
type ScenarioRepo interface {
	Create(ctx context.Context, s *domain.Scenario) error // заполняет ID; ErrDuplicate при занятом ключе
	GetByKey(ctx context.Context, key string) (*domain.Scenario, error)
	GetByID(ctx context.Context, id int64) (*domain.Scenario, error)
	// UpdateHead обновляет копию названия и оператора (для списков) после новой версии.
	UpdateHead(ctx context.Context, id int64, title, operator string, at time.Time) error
	SetArchived(ctx context.Context, id int64, archived bool, at time.Time) error
	List(ctx context.Context, includeArchived bool, limit, offset int) ([]*domain.Scenario, error)
	// AddVersion добавляет версию со следующим номером и заполняет ID и Version.
	AddVersion(ctx context.Context, v *domain.ScenarioVersion) error
	GetVersion(ctx context.Context, id int64) (*domain.ScenarioVersion, error)
	LatestVersion(ctx context.Context, scenarioID int64) (*domain.ScenarioVersion, error)
}

// Как учитывать задания в статусе «проверено» в выборке (по состоянию компенсации).
const (
	ReviewedNone     = 0 // не включать
	ReviewedOpenComp = 1 // включать, если компенсация ещё не закрыта (к выплате или отклонена)
	ReviewedSettled  = 2 // включать, если компенсации нет или она выплачена
)

// TaskFilter условия выборки заданий (нулевые значения = без фильтра).
// Statuses и ReviewedMode объединяются через ИЛИ; если заданы оба пустыми, фильтра по статусу нет.
type TaskFilter struct {
	UserID       int64
	ScenarioID   int64
	Statuses     []domain.TaskStatus
	ReviewedMode int
	// DueBefore: только принятые задания, срок которых вышел к этому моменту.
	DueBefore time.Time
}

// TaskRepo задания и их история.
type TaskRepo interface {
	Create(ctx context.Context, t *domain.Task) error // заполняет ID
	Get(ctx context.Context, id int64) (*domain.Task, error)
	List(ctx context.Context, f TaskFilter, limit, offset int) ([]*domain.Task, error)
	Count(ctx context.Context, f TaskFilter) (int, error)
	// Transition атомарно меняет статус, только если он сейчас равен from.
	// Возвращает false, если статус уже другой (например, повторное нажатие кнопки).
	// at пишется в поле времени нового статуса; dueAt используется при переходе в accepted.
	Transition(ctx context.Context, id int64, from, to domain.TaskStatus, at, dueAt time.Time) (bool, error)
	AddEvent(ctx context.Context, e *domain.TaskEvent) error
	Events(ctx context.Context, taskID int64) ([]*domain.TaskEvent, error)
	// DeleteUnstarted полностью удаляет задание вместе с историей, но только в статусах
	// created, sent, declined (у них нет ни промокода, ни отчёта). false, если статус другой.
	DeleteUnstarted(ctx context.Context, id int64) (bool, error)
}

// SettingsRepo настройки приложения (ключ-значение).
type SettingsRepo interface {
	Get(ctx context.Context, key string) (value string, ok bool, err error)
	Set(ctx context.Context, key, value string, at time.Time) error
	All(ctx context.Context) (map[string]string, error)
}

// ReminderRepo журнал напоминаний.
type ReminderRepo interface {
	// Seqs возвращает уже записанные номера (0 = эскалация) для задания, вида и базового момента.
	Seqs(ctx context.Context, taskID int64, kind string, baseAt time.Time) (map[int]bool, error)
	// Record записывает напоминание. false, если такая запись уже есть (повторный запуск не дублирует).
	Record(ctx context.Context, taskID int64, kind string, baseAt time.Time, seq int, skipped bool, at time.Time) (bool, error)
	// Sent сколько напоминаний покупателю реально отправлено (без пропущенных и эскалации).
	Sent(ctx context.Context, taskID int64, kind string, baseAt time.Time) (int, error)
}

// FSMRepo состояние пошаговых диалогов.
type FSMRepo interface {
	Get(ctx context.Context, userID int64) (state, data string, err error) // ErrNotFound, если нет
	Set(ctx context.Context, userID int64, state, data string, at time.Time) error
	Clear(ctx context.Context, userID int64) error
}

// PromoRow строка таблицы кодов для админа.
type PromoRow struct {
	Code         string
	UsedCount    int
	ActiveTaskID int64
}

// PromoRepo пул промокодов. maxUses везде общий лимит использований одного кода.
type PromoRepo interface {
	// AddBatch добавляет коды, уже существующие пропускает. Возвращает число добавленных.
	AddBatch(ctx context.Context, codes []string, addedBy int64, at time.Time) (int, error)
	// Issue выдаёт заданию свободный код с остатком использований. Вызывать внутри транзакции.
	// Если заданию код уже выдан, возвращает его. ErrNotFound, если подходящего кода нет.
	Issue(ctx context.Context, taskID, userID int64, maxUses int, at time.Time) (*domain.PromoAssignment, error)
	// ByTask выдача по заданию (ErrNotFound, если кода не выдавали).
	ByTask(ctx context.Context, taskID int64) (*domain.PromoAssignment, error)
	// Release завершает активную выдачу задания: used=true засчитывает использование,
	// false просто возвращает код в оборот. false, если активной выдачи нет.
	Release(ctx context.Context, taskID int64, used bool, at time.Time) (bool, error)
	Stats(ctx context.Context, maxUses int) (domain.PromoStats, error)
	// List страница таблицы кодов (незанятые и занятые вперемешку, по порядку добавления).
	List(ctx context.Context, limit, offset int) ([]PromoRow, int, error)
	// DeleteFree удаляет из списка только коды, не занятые активными заданиями.
	DeleteFree(ctx context.Context, codes []string) (int, error)
	// DeleteAllFree удаляет все коды, не занятые активными заданиями.
	DeleteAllFree(ctx context.Context) (int, error)
}

// ReportRepo отчёты и ответы.
type ReportRepo interface {
	// Create сохраняет новую версию отчёта (заполняет ID и Revision: 1 для первого, далее по порядку).
	Create(ctx context.Context, r *domain.Report) error
	// ByTask последняя версия отчёта по заданию (ErrNotFound, если отчётов нет).
	ByTask(ctx context.Context, taskID int64) (*domain.Report, error)
	// SetDecision записывает решение админа и комментарий в последнюю версию отчёта.
	SetDecision(ctx context.Context, taskID int64, decision, comment string, by int64, at time.Time) error
}

// CompRepo компенсации.
type CompRepo interface {
	Create(ctx context.Context, c *domain.Compensation) error
	Get(ctx context.Context, id int64) (*domain.Compensation, error)
	ByTask(ctx context.Context, taskID int64) (*domain.Compensation, error)
	List(ctx context.Context, status domain.CompStatus, limit, offset int) ([]*domain.Compensation, int, error)
	SumByStatus(ctx context.Context, status domain.CompStatus) (int64, error)
	// MarkPaid ставит «выплачено», только если статус ещё pending.
	MarkPaid(ctx context.Context, id, by int64, at time.Time) (bool, error)
	// Reject отклоняет компенсацию (только из pending) с комментарием для покупателя.
	Reject(ctx context.Context, id int64, comment string) (bool, error)
	// UpdateData заменяет сумму и чек у компенсации, которую ещё не выплатили, и возвращает её в pending.
	UpdateData(ctx context.Context, taskID, amount int64, fileID, uniqueID string) (bool, error)
	// FindByReceipt ищет другое задание с тем же файлом чека (защита от повторного использования).
	FindByReceipt(ctx context.Context, uniqueID string, exceptTaskID int64) (int64, bool, error)
}

// ExportFilter условия выборки для статистики и выгрузок (нулевые значения = без фильтра).
// Период считается по дате создания задания, To не включается.
type ExportFilter struct {
	From, To   time.Time
	ScenarioID int64
	Operator   string
}

// ExportRow одно задание со всем нужным для статистики и CSV: покупатель, сценарий, время этапов,
// последняя версия отчёта с ответами, компенсация и промокод.
type ExportRow struct {
	TaskID     int64
	UserID     int64
	UserName   string
	Username   string
	ScenarioID int64
	Key        string
	Title      string
	Operator   string
	Version    int
	Status     domain.TaskStatus

	CreatedAt, SentAt, AcceptedAt, DueAt, DeclinedAt, ReportedAt, ReviewedAt time.Time

	Revision int // версия отчёта (0, если отчёта нет)
	Late     bool
	Decision string
	Answers  []domain.Answer

	CompStatus domain.CompStatus // пусто, если компенсации нет
	CompAmount int64
	CompPaidAt time.Time

	PromoCode string
	Reminders int // сколько напоминаний отправлено покупателю
}

// StatsRepo выборки для статистики и выгрузок.
type StatsRepo interface {
	// Export возвращает задания (от новых к старым) с отчётом последней версии.
	Export(ctx context.Context, f ExportFilter) ([]ExportRow, error)
}

// Repos набор всех репозиториев.
type Repos struct {
	Users     UserRepo
	Invites   InviteRepo
	Audit     AuditRepo
	Scenarios ScenarioRepo
	Tasks     TaskRepo
	FSM       FSMRepo
	Promos    PromoRepo
	Reports   ReportRepo
	Comps     CompRepo
	Settings  SettingsRepo
	Reminders ReminderRepo
	Stats     StatsRepo
}

// Store хранилище: репозитории + транзакции.
type Store interface {
	// Repos возвращает репозитории для работы вне транзакции.
	Repos() Repos
	// WithTx выполняет fn в одной транзакции (commit при nil, иначе rollback).
	WithTx(ctx context.Context, fn func(r Repos) error) error
	// Backup сохраняет согласованный снимок базы в файл dest (файл не должен существовать).
	Backup(ctx context.Context, dest string) error
	Close() error
}
