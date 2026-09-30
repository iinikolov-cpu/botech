package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"botech/internal/domain"
	"botech/internal/storage"
)

// Ограничения отчёта.
const (
	MaxTextAnswer = 1000
	MaxCompAmount = 100_000_000 // сум; защита от опечаток
)

// Ошибки отчёта.
var (
	ErrAlreadyReported = errors.New("отчёт по этому заданию уже отправлен")
	ErrInvalidReport   = errors.New("отчёт заполнен неверно")
)

// CompInput данные для компенсации из отчёта.
type CompInput struct {
	Amount          int64
	ReceiptFileID   string
	ReceiptUniqueID string
}

// SubmitInput содержимое отчёта.
type SubmitInput struct {
	Answers []domain.Answer
	Comp    *CompInput // nil, если компенсация не нужна
}

// SubmitResult итог отправки отчёта.
type SubmitResult struct {
	Card       *TaskCard
	Report     *domain.Report
	Late       bool
	DupReceipt int64 // задание, в котором уже был такой же файл чека (0, если нет)
}

// Reports принимает отчёты и ведёт компенсации.
type Reports struct {
	store storage.Store
	now   func() time.Time
	tasks *Tasks
}

// NewReports создаёт сервис отчётов.
func NewReports(store storage.Store, tasks *Tasks) *Reports {
	return &Reports{store: store, now: time.Now, tasks: tasks}
}

// CanReport проверяет, что покупатель может сейчас отправить отчёт по заданию.
func (s *Reports) CanReport(ctx context.Context, userID, taskID int64) (*TaskCard, error) {
	card, err := s.tasks.Card(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if card.Task.UserID != userID {
		return nil, ErrNotFound
	}
	switch card.Task.Status {
	case domain.TaskAccepted, domain.TaskExpired:
		return card, nil
	case domain.TaskReported, domain.TaskReviewed:
		return card, ErrAlreadyReported
	case domain.TaskCancelled:
		return card, fmt.Errorf("%w: задание отменено администратором", ErrForbidden)
	}
	return card, fmt.Errorf("%w: отчёт можно отправить после принятия задания", ErrForbidden)
}

// ValidateAnswers проверяет ответы по чек-листу версии сценария и возвращает их в порядке вопросов.
// Правила проверяются на сервере независимо от того, что показал интерфейс.
func ValidateAnswers(body domain.ScenarioBody, answers []domain.Answer) ([]domain.Answer, error) {
	byKey := make(map[string]domain.Answer, len(answers))
	for _, a := range answers {
		byKey[a.Key] = a
	}
	known := make(map[string]bool, len(body.Questions))
	out := make([]domain.Answer, 0, len(body.Questions))
	for i, q := range body.Questions {
		known[q.Key] = true
		a, ok := byKey[q.Key]
		if !ok || a.Skipped {
			if q.Required {
				return nil, fmt.Errorf("%w: вопрос %d обязателен", ErrInvalidReport, i+1)
			}
			out = append(out, domain.Answer{Key: q.Key, Type: q.Type, Skipped: true})
			continue
		}
		a.Type = q.Type // тип берём из сценария, а не от клиента
		switch q.Type {
		case domain.QText:
			a.Value = strings.TrimSpace(a.Value)
			if a.Value == "" || utf8.RuneCountInString(a.Value) > MaxTextAnswer {
				return nil, fmt.Errorf("%w: вопрос %d: текст от 1 до %d символов", ErrInvalidReport, i+1, MaxTextAnswer)
			}
		case domain.QRating:
			if a.Value != "1" && a.Value != "2" && a.Value != "3" && a.Value != "4" && a.Value != "5" {
				return nil, fmt.Errorf("%w: вопрос %d: оценка от 1 до 5", ErrInvalidReport, i+1)
			}
		case domain.QYesNo:
			if a.Value != "yes" && a.Value != "no" {
				return nil, fmt.Errorf("%w: вопрос %d: ответ да или нет", ErrInvalidReport, i+1)
			}
		case domain.QPhoto, domain.QVideo:
			if a.FileID == "" {
				return nil, fmt.Errorf("%w: вопрос %d: нужен файл", ErrInvalidReport, i+1)
			}
		}
		out = append(out, a)
	}
	for k := range byKey {
		if !known[k] {
			return nil, fmt.Errorf("%w: лишний ответ %q", ErrInvalidReport, k)
		}
	}
	return out, nil
}

// Submit сохраняет отчёт и компенсацию и переводит задание в «отчёт получен» одной транзакцией.
// Срок не блокирует отправку: опоздавший отчёт принимается и помечается.
func (s *Reports) Submit(ctx context.Context, userID, taskID int64, in SubmitInput) (*SubmitResult, error) {
	res := &SubmitResult{}
	err := s.store.WithTx(ctx, func(r storage.Repos) error {
		t, err := r.Tasks.Get(ctx, taskID)
		if errors.Is(err, storage.ErrNotFound) || (err == nil && t.UserID != userID) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch t.Status {
		case domain.TaskAccepted, domain.TaskExpired:
		case domain.TaskReported, domain.TaskReviewed:
			return ErrAlreadyReported
		case domain.TaskCancelled:
			return fmt.Errorf("%w: задание отменено администратором", ErrForbidden)
		default:
			return fmt.Errorf("%w: отчёт можно отправить после принятия задания", ErrForbidden)
		}
		ver, err := r.Scenarios.GetVersion(ctx, t.ScenarioVersionID)
		if err != nil {
			return err
		}
		answers, err := ValidateAnswers(ver.Body, in.Answers)
		if err != nil {
			return err
		}
		if in.Comp != nil {
			if in.Comp.Amount < 1 || in.Comp.Amount > MaxCompAmount {
				return fmt.Errorf("%w: сумма должна быть от 1 до %d", ErrInvalidReport, MaxCompAmount)
			}
			if in.Comp.ReceiptFileID == "" {
				return fmt.Errorf("%w: нужно фото чека", ErrInvalidReport)
			}
		}

		now := s.now().UTC()
		late := t.Status == domain.TaskExpired || (!t.DueAt.IsZero() && now.After(t.DueAt))
		rep := &domain.Report{TaskID: taskID, UserID: userID, Late: late, SubmittedAt: now, Answers: answers}
		if err := r.Reports.Create(ctx, rep); err != nil {
			if errors.Is(err, storage.ErrDuplicate) {
				return ErrAlreadyReported
			}
			return err
		}
		if in.Comp != nil {
			c := &domain.Compensation{
				TaskID: taskID, UserID: userID, Amount: in.Comp.Amount,
				ReceiptFileID: in.Comp.ReceiptFileID, ReceiptUniqueID: in.Comp.ReceiptUniqueID, CreatedAt: now,
			}
			if err := r.Comps.Create(ctx, c); err != nil {
				return err
			}
			if dup, found, err := r.Comps.FindByReceipt(ctx, c.ReceiptUniqueID, taskID); err != nil {
				return err
			} else if found {
				res.DupReceipt = dup
			}
		}
		ok, err := r.Tasks.Transition(ctx, taskID, t.Status, domain.TaskReported, now, time.Time{})
		if err != nil {
			return err
		}
		if !ok {
			return ErrAlreadyReported // параллельный запрос успел раньше
		}
		details := ""
		if late {
			details = "с опозданием"
		}
		res.Late, res.Report = late, rep
		return r.Tasks.AddEvent(ctx, &domain.TaskEvent{
			TaskID: taskID, Kind: "status", FromStatus: t.Status, ToStatus: domain.TaskReported, ActorID: userID, Details: details, At: now,
		})
	})
	if err != nil {
		return nil, err
	}
	res.Card, err = s.tasks.Card(ctx, taskID)
	return res, err
}

// Get отчёт и компенсация по заданию (nil, если нет).
func (s *Reports) Get(ctx context.Context, taskID int64) (*domain.Report, error) {
	rep, err := s.store.Repos().Reports.ByTask(ctx, taskID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	return rep, err
}

// CompCard компенсация со связанными данными для админа.
type CompCard struct {
	Comp       *domain.Compensation
	User       *domain.User
	Title      string
	DupReceipt int64 // другое задание с тем же файлом чека
}

func (s *Reports) compCard(ctx context.Context, r storage.Repos, c *domain.Compensation) (*CompCard, error) {
	u, err := r.Users.Get(ctx, c.UserID)
	if err != nil {
		return nil, err
	}
	t, err := r.Tasks.Get(ctx, c.TaskID)
	if err != nil {
		return nil, err
	}
	v, err := r.Scenarios.GetVersion(ctx, t.ScenarioVersionID)
	if err != nil {
		return nil, err
	}
	dup, _, err := r.Comps.FindByReceipt(ctx, c.ReceiptUniqueID, c.TaskID)
	return &CompCard{Comp: c, User: u, Title: v.Body.Title, DupReceipt: dup}, err
}

// CompPage страница компенсаций с общим числом и суммой по статусу.
func (s *Reports) CompPage(ctx context.Context, status domain.CompStatus, limit, offset int) ([]*CompCard, int, int64, error) {
	r := s.store.Repos()
	list, total, err := r.Comps.List(ctx, status, limit, offset)
	if err != nil {
		return nil, 0, 0, err
	}
	sum, err := r.Comps.SumByStatus(ctx, status)
	if err != nil {
		return nil, 0, 0, err
	}
	out := make([]*CompCard, 0, len(list))
	for _, c := range list {
		cc, err := s.compCard(ctx, r, c)
		if err != nil {
			return nil, 0, 0, err
		}
		out = append(out, cc)
	}
	return out, total, sum, nil
}

// CompCardByID карточка одной компенсации.
func (s *Reports) CompCardByID(ctx context.Context, id int64) (*CompCard, error) {
	r := s.store.Repos()
	c, err := r.Comps.Get(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.compCard(ctx, r, c)
}

// MarkPaid отмечает компенсацию выплаченной (только из «к выплате»).
func (s *Reports) MarkPaid(ctx context.Context, admin, id int64) (bool, error) {
	changed := false
	err := s.store.WithTx(ctx, func(r storage.Repos) error {
		now := s.now().UTC()
		ok, err := r.Comps.MarkPaid(ctx, id, admin, now)
		if err != nil || !ok {
			return err
		}
		changed = true
		return r.Audit.Add(ctx, &domain.AuditEntry{
			AdminID: admin, Action: "compensation.paid", Entity: "compensation", EntityID: fmt.Sprint(id), At: now,
		})
	})
	return changed, err
}
