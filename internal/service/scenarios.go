package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"botech/internal/domain"
	"botech/internal/scenario"
	"botech/internal/storage"
)

// Scenarios управляет сценариями и их версиями.
type Scenarios struct {
	store storage.Store
	now   func() time.Time
}

// NewScenarios создаёт сервис сценариев.
func NewScenarios(store storage.Store) *Scenarios {
	return &Scenarios{store: store, now: time.Now}
}

// ImportResult итог загрузки файла сценария.
type ImportResult struct {
	Scenario  *domain.Scenario
	Version   *domain.ScenarioVersion
	Created   bool // сценарий создан впервые
	Unchanged bool // содержимое совпало с последней версией, новая версия не создавалась
	Restored  bool // сценарий был в архиве и возвращён в работу
}

// Import разбирает файл и создаёт сценарий или его новую версию.
// Ошибки проверки файла возвращаются списком (validation), системные ошибки в err.
func (s *Scenarios) Import(ctx context.Context, actor int64, data []byte) (res *ImportResult, validation []string, err error) {
	parsed, errs := scenario.Parse(data)
	if len(errs) > 0 {
		return nil, errs, nil
	}
	now := s.now().UTC()
	err = s.store.WithTx(ctx, func(r storage.Repos) error {
		res = &ImportResult{}
		sc, err := r.Scenarios.GetByKey(ctx, parsed.Key)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			sc = &domain.Scenario{
				Key: parsed.Key, Title: parsed.Body.Title, Operator: parsed.Body.Operator,
				CreatedBy: actor, CreatedAt: now, UpdatedAt: now,
			}
			if err := r.Scenarios.Create(ctx, sc); err != nil {
				return err
			}
			res.Created = true
		case err != nil:
			return err
		default:
			last, err := r.Scenarios.LatestVersion(ctx, sc.ID)
			if err != nil {
				return err
			}
			if reflect.DeepEqual(last.Body, parsed.Body) {
				res.Scenario, res.Version, res.Unchanged = sc, last, true
				return nil
			}
		}
		v := &domain.ScenarioVersion{ScenarioID: sc.ID, Body: parsed.Body, CreatedBy: actor, CreatedAt: now}
		if err := r.Scenarios.AddVersion(ctx, v); err != nil {
			return err
		}
		if err := r.Scenarios.UpdateHead(ctx, sc.ID, parsed.Body.Title, parsed.Body.Operator, now); err != nil {
			return err
		}
		if sc.Archived {
			if err := r.Scenarios.SetArchived(ctx, sc.ID, false, now); err != nil {
				return err
			}
			res.Restored = true
		}
		sc.Title, sc.Operator, sc.LatestVersion, sc.Archived = parsed.Body.Title, parsed.Body.Operator, v.Version, false
		res.Scenario, res.Version = sc, v
		return r.Audit.Add(ctx, &domain.AuditEntry{
			AdminID: actor, Action: "scenario.import", Entity: "scenario", EntityID: sc.Key,
			Details: fmt.Sprintf("v%d", v.Version), At: now,
		})
	})
	if err != nil {
		return nil, nil, err
	}
	return res, nil, nil
}

// List сценарии для списков.
func (s *Scenarios) List(ctx context.Context, includeArchived bool) ([]*domain.Scenario, error) {
	return s.store.Repos().Scenarios.List(ctx, includeArchived, 50, 0)
}

// Get сценарий и его последняя версия.
func (s *Scenarios) Get(ctx context.Context, id int64) (*domain.Scenario, *domain.ScenarioVersion, error) {
	r := s.store.Repos()
	sc, err := r.Scenarios.GetByID(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	v, err := r.Scenarios.LatestVersion(ctx, id)
	return sc, v, err
}

// SetArchived убирает сценарий из выбора при назначении (или возвращает). Выданные задания не затрагиваются.
func (s *Scenarios) SetArchived(ctx context.Context, actor, id int64, archived bool) error {
	return s.store.WithTx(ctx, func(r storage.Repos) error {
		sc, err := r.Scenarios.GetByID(ctx, id)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		if err := r.Scenarios.SetArchived(ctx, id, archived, now); err != nil {
			return err
		}
		action := "scenario.archive"
		if !archived {
			action = "scenario.restore"
		}
		return r.Audit.Add(ctx, &domain.AuditEntry{
			AdminID: actor, Action: action, Entity: "scenario", EntityID: sc.Key, At: now,
		})
	})
}
