-- Добавляем статус «cancelled» (отменено админом). SQLite не умеет менять CHECK на месте,
-- поэтому таблицу пересоздаём. Внешние ключи на время миграции отключены (см. applyMigrations).
CREATE TABLE tasks_new (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT, -- номера не переиспользуются после удаления
    scenario_id         INTEGER NOT NULL REFERENCES scenarios (id),
    scenario_version_id INTEGER NOT NULL REFERENCES scenario_versions (id),
    user_id             INTEGER NOT NULL REFERENCES users (tg_id),
    status              TEXT    NOT NULL CHECK (status IN
        ('created', 'sent', 'accepted', 'declined', 'reported', 'expired', 'reviewed', 'cancelled')),
    due_days            INTEGER NOT NULL,
    created_by          INTEGER NOT NULL,
    created_at          INTEGER NOT NULL,
    sent_at             INTEGER NOT NULL DEFAULT 0,
    accepted_at         INTEGER NOT NULL DEFAULT 0,
    due_at              INTEGER NOT NULL DEFAULT 0,
    declined_at         INTEGER NOT NULL DEFAULT 0,
    reported_at         INTEGER NOT NULL DEFAULT 0,
    reviewed_at         INTEGER NOT NULL DEFAULT 0
);

INSERT INTO tasks_new (id, scenario_id, scenario_version_id, user_id, status, due_days, created_by,
                       created_at, sent_at, accepted_at, due_at, declined_at, reported_at, reviewed_at)
SELECT id, scenario_id, scenario_version_id, user_id, status, due_days, created_by,
       created_at, sent_at, accepted_at, due_at, declined_at, reported_at, reviewed_at
  FROM tasks;

DROP TABLE tasks;
ALTER TABLE tasks_new RENAME TO tasks;

CREATE INDEX idx_tasks_user   ON tasks (user_id, status);
CREATE INDEX idx_tasks_status ON tasks (status);
CREATE UNIQUE INDEX uq_tasks_active ON tasks (user_id, scenario_id)
    WHERE status IN ('created', 'sent', 'accepted', 'expired');
