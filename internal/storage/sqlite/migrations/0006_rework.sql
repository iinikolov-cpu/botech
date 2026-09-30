-- Возврат отчёта на доработку и отклонение компенсации.
-- 1) tasks: новый статус 'rework'; уникальный индекс «одно активное задание на сценарий»
--    убран, одно и то же задание можно назначать одному покупателю повторно.
-- 2) reports: несколько версий отчёта по заданию (после доработки), решение и комментарий админа.
-- 3) compensations: новый статус 'rejected' и комментарий админа.
-- Таблицы пересоздаются (SQLite не меняет CHECK на месте), внешние ключи на время миграции
-- отключены (см. applyMigrations).

CREATE TEMP TABLE _task_seq AS
SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'tasks'), 0) AS seq;

CREATE TABLE tasks_new (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    scenario_id         INTEGER NOT NULL REFERENCES scenarios (id),
    scenario_version_id INTEGER NOT NULL REFERENCES scenario_versions (id),
    user_id             INTEGER NOT NULL REFERENCES users (tg_id),
    status              TEXT    NOT NULL CHECK (status IN
        ('created', 'sent', 'accepted', 'declined', 'reported', 'expired', 'reviewed', 'cancelled', 'rework')),
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
-- Номера заданий не переиспользуются: сохраняем счётчик, даже если последние задания удалены.
UPDATE sqlite_sequence SET seq = MAX(seq, (SELECT seq FROM _task_seq)) WHERE name = 'tasks';
INSERT INTO sqlite_sequence (name, seq)
SELECT 'tasks', seq FROM _task_seq
 WHERE seq > 0 AND NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'tasks');
DROP TABLE _task_seq;

CREATE TABLE reports_new (
    id            INTEGER PRIMARY KEY,
    task_id       INTEGER NOT NULL REFERENCES tasks (id),
    user_id       INTEGER NOT NULL,
    revision      INTEGER NOT NULL DEFAULT 1,
    late          INTEGER NOT NULL DEFAULT 0,
    submitted_at  INTEGER NOT NULL,
    decision      TEXT    NOT NULL DEFAULT '' CHECK (decision IN ('', 'accepted', 'rework')),
    admin_comment TEXT    NOT NULL DEFAULT '',
    decided_by    INTEGER NOT NULL DEFAULT 0,
    decided_at    INTEGER NOT NULL DEFAULT 0,
    UNIQUE (task_id, revision)
);
INSERT INTO reports_new (id, task_id, user_id, revision, late, submitted_at, decision)
SELECT r.id, r.task_id, r.user_id, 1, r.late, r.submitted_at,
       CASE WHEN (SELECT status FROM tasks t WHERE t.id = r.task_id) = 'reviewed' THEN 'accepted' ELSE '' END
  FROM reports r;
DROP TABLE reports;
ALTER TABLE reports_new RENAME TO reports;

CREATE TABLE compensations_new (
    id                INTEGER PRIMARY KEY,
    task_id           INTEGER NOT NULL UNIQUE REFERENCES tasks (id),
    user_id           INTEGER NOT NULL,
    amount            INTEGER NOT NULL,
    receipt_file_id   TEXT    NOT NULL,
    receipt_unique_id TEXT    NOT NULL,
    status            TEXT    NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'paid', 'rejected')),
    admin_comment     TEXT    NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,
    paid_at           INTEGER NOT NULL DEFAULT 0,
    paid_by           INTEGER NOT NULL DEFAULT 0
);
INSERT INTO compensations_new (id, task_id, user_id, amount, receipt_file_id, receipt_unique_id, status, created_at, paid_at, paid_by)
SELECT id, task_id, user_id, amount, receipt_file_id, receipt_unique_id, status, created_at, paid_at, paid_by
  FROM compensations;
DROP TABLE compensations;
ALTER TABLE compensations_new RENAME TO compensations;
CREATE INDEX idx_comp_status  ON compensations (status, id);
CREATE INDEX idx_comp_receipt ON compensations (receipt_unique_id);
