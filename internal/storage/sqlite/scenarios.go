package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"botech/internal/domain"
	"botech/internal/storage"
)

type scenarioRepo struct{ q dbtx }

// isUnique распознаёт нарушение уникальности SQLite.
func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

const scenarioCols = `s.id, s.key, s.kind, s.title, s.operator, s.archived, s.created_by, s.created_at, s.updated_at,
	COALESCE((SELECT MAX(version) FROM scenario_versions v WHERE v.scenario_id = s.id), 0)`

func scanScenario(sc interface{ Scan(...any) error }) (*domain.Scenario, error) {
	var (
		s        domain.Scenario
		kind     string
		arch     int64
		cr, upAt int64
	)
	if err := sc.Scan(&s.ID, &s.Key, &kind, &s.Title, &s.Operator, &arch, &s.CreatedBy, &cr, &upAt, &s.LatestVersion); err != nil {
		return nil, err
	}
	s.Kind = domain.TaskKind(kind)
	s.Archived = arch != 0
	s.CreatedAt, s.UpdatedAt = time.Unix(cr, 0).UTC(), time.Unix(upAt, 0).UTC()
	return &s, nil
}

func (r *scenarioRepo) one(ctx context.Context, where string, arg any) (*domain.Scenario, error) {
	s, err := scanScenario(r.q.QueryRowContext(ctx, `SELECT `+scenarioCols+` FROM scenarios s WHERE `+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	return s, err
}

func (r *scenarioRepo) GetByKey(ctx context.Context, key string) (*domain.Scenario, error) {
	return r.one(ctx, "s.key = ?", key)
}

func (r *scenarioRepo) GetByID(ctx context.Context, id int64) (*domain.Scenario, error) {
	return r.one(ctx, "s.id = ?", id)
}

func (r *scenarioRepo) Create(ctx context.Context, s *domain.Scenario) error {
	err := r.q.QueryRowContext(ctx,
		`INSERT INTO scenarios (key, kind, title, operator, archived, created_by, created_at, updated_at)
		 VALUES (?,?,?,?,0,?,?,?) RETURNING id`,
		s.Key, s.Kind, s.Title, s.Operator, s.CreatedBy, s.CreatedAt.Unix(), s.UpdatedAt.Unix()).Scan(&s.ID)
	if isUnique(err) {
		return storage.ErrDuplicate
	}
	return err
}

func (r *scenarioRepo) UpdateHead(ctx context.Context, id int64, title, operator string, at time.Time) error {
	_, err := r.q.ExecContext(ctx, `UPDATE scenarios SET title=?, operator=?, updated_at=? WHERE id=?`,
		title, operator, at.Unix(), id)
	return err
}

func (r *scenarioRepo) SetArchived(ctx context.Context, id int64, archived bool, at time.Time) error {
	v := 0
	if archived {
		v = 1
	}
	_, err := r.q.ExecContext(ctx, `UPDATE scenarios SET archived=?, updated_at=? WHERE id=?`, v, at.Unix(), id)
	return err
}

func (r *scenarioRepo) List(ctx context.Context, includeArchived bool, limit, offset int) ([]*domain.Scenario, error) {
	cond := "s.archived = 0"
	if includeArchived {
		cond = "1=1"
	}
	rows, err := r.q.QueryContext(ctx,
		`SELECT `+scenarioCols+` FROM scenarios s WHERE `+cond+` ORDER BY s.title, s.id LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Scenario
	for rows.Next() {
		s, err := scanScenario(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *scenarioRepo) AddVersion(ctx context.Context, v *domain.ScenarioVersion) error {
	body, err := json.Marshal(v.Body)
	if err != nil {
		return err
	}
	// Номер версии считается тем же запросом, что и вставка: гонок нет (запись сериализуется БД).
	return r.q.QueryRowContext(ctx,
		`INSERT INTO scenario_versions (scenario_id, version, body, created_by, created_at)
		 SELECT ?, COALESCE(MAX(version), 0) + 1, ?, ?, ? FROM scenario_versions WHERE scenario_id = ?
		 RETURNING id, version`,
		v.ScenarioID, string(body), v.CreatedBy, v.CreatedAt.Unix(), v.ScenarioID).Scan(&v.ID, &v.Version)
}

func scanVersion(sc interface{ Scan(...any) error }) (*domain.ScenarioVersion, error) {
	var (
		v    domain.ScenarioVersion
		body string
		cr   int64
	)
	if err := sc.Scan(&v.ID, &v.ScenarioID, &v.Version, &body, &v.CreatedBy, &cr); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(body), &v.Body); err != nil {
		return nil, err
	}
	if v.Body.Kind == "" { // версии, созданные до появления типов сценариев
		v.Body.Kind = domain.KindBuyer
	}
	v.CreatedAt = time.Unix(cr, 0).UTC()
	return &v, nil
}

const versionCols = `id, scenario_id, version, body, created_by, created_at`

func (r *scenarioRepo) GetVersion(ctx context.Context, id int64) (*domain.ScenarioVersion, error) {
	v, err := scanVersion(r.q.QueryRowContext(ctx, `SELECT `+versionCols+` FROM scenario_versions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	return v, err
}

func (r *scenarioRepo) LatestVersion(ctx context.Context, scenarioID int64) (*domain.ScenarioVersion, error) {
	v, err := scanVersion(r.q.QueryRowContext(ctx,
		`SELECT `+versionCols+` FROM scenario_versions WHERE scenario_id = ? ORDER BY version DESC LIMIT 1`, scenarioID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	return v, err
}
