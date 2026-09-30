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

// TaskFilter условия выборки заданий (нулевые значения = без фильтра).
type TaskFilter struct {
	UserID   int64
	Statuses []domain.TaskStatus
}

// TaskRepo задания и их история.
type TaskRepo interface {
	Create(ctx context.Context, t *domain.Task) error // заполняет ID; ErrDuplicate, если уже есть активное
	Get(ctx context.Context, id int64) (*domain.Task, error)
	List(ctx context.Context, f TaskFilter, limit, offset int) ([]*domain.Task, error)
	Count(ctx context.Context, f TaskFilter) (int, error)
	// Transition атомарно меняет статус, только если он сейчас равен from.
	// Возвращает false, если статус уже другой (например, повторное нажатие кнопки).
	// at пишется в поле времени нового статуса; dueAt используется при переходе в accepted.
	Transition(ctx context.Context, id int64, from, to domain.TaskStatus, at, dueAt time.Time) (bool, error)
	AddEvent(ctx context.Context, e *domain.TaskEvent) error
	Events(ctx context.Context, taskID int64) ([]*domain.TaskEvent, error)
}

// FSMRepo состояние пошаговых диалогов.
type FSMRepo interface {
	Get(ctx context.Context, userID int64) (state, data string, err error) // ErrNotFound, если нет
	Set(ctx context.Context, userID int64, state, data string, at time.Time) error
	Clear(ctx context.Context, userID int64) error
}

// PromoRepo пул промокодов.
type PromoRepo interface {
	// AddBatch добавляет коды, уже существующие пропускает. Возвращает число добавленных.
	AddBatch(ctx context.Context, codes []string, addedBy int64, at time.Time) (int, error)
	// Issue атомарно выдаёт свободный код заданию. ErrNotFound, если пул пуст.
	// Если заданию код уже выдан, возвращает его (повторной выдачи нет).
	Issue(ctx context.Context, taskID, userID int64, at time.Time) (*domain.PromoCode, error)
	ByTask(ctx context.Context, taskID int64) (*domain.PromoCode, error) // ErrNotFound, если не выдан
	Get(ctx context.Context, id int64) (*domain.PromoCode, error)
	Stats(ctx context.Context) (domain.PromoStats, error)
	// ListIssued выданные коды: used=false только неиспользованные, true только использованные.
	ListIssued(ctx context.Context, used bool, limit, offset int) ([]*domain.PromoCode, int, error)
	MarkUsed(ctx context.Context, id, by int64, at time.Time) (bool, error)
	// DeleteFree удаляет только свободные коды из списка (выданные и использованные не трогает).
	DeleteFree(ctx context.Context, codes []string) (int, error)
	// DeleteAllFree удаляет все свободные коды.
	DeleteAllFree(ctx context.Context) (int, error)
}

// ReportRepo отчёты и ответы.
type ReportRepo interface {
	Create(ctx context.Context, r *domain.Report) error // ErrDuplicate, если отчёт по заданию уже есть
	ByTask(ctx context.Context, taskID int64) (*domain.Report, error)
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
	// FindByReceipt ищет другое задание с тем же файлом чека (защита от повторного использования).
	FindByReceipt(ctx context.Context, uniqueID string, exceptTaskID int64) (int64, bool, error)
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
}

// Store хранилище: репозитории + транзакции.
type Store interface {
	// Repos возвращает репозитории для работы вне транзакции.
	Repos() Repos
	// WithTx выполняет fn в одной транзакции (commit при nil, иначе rollback).
	WithTx(ctx context.Context, fn func(r Repos) error) error
	Close() error
}
