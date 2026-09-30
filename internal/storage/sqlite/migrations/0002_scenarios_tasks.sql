-- Сценарии: здесь только ключ и признак архива, а содержимое живёт в версиях.
CREATE TABLE scenarios (
    id         INTEGER PRIMARY KEY,
    key        TEXT    NOT NULL UNIQUE,
    title      TEXT    NOT NULL,   -- копия названия последней версии, для списков
    operator   TEXT    NOT NULL,   -- копия оператора последней версии, для списков
    archived   INTEGER NOT NULL DEFAULT 0,
    created_by INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- Версии сценария неизменяемы: правка файла создаёт новую строку.
CREATE TABLE scenario_versions (
    id          INTEGER PRIMARY KEY,
    scenario_id INTEGER NOT NULL REFERENCES scenarios (id),
    version     INTEGER NOT NULL,
    body        TEXT    NOT NULL,  -- JSON: название, оператор, шаги, вопросы
    created_by  INTEGER NOT NULL,
    created_at  INTEGER NOT NULL,
    UNIQUE (scenario_id, version)
);

-- Задания. Время хранится как unix-секунды UTC, 0 = ещё не наступило.
CREATE TABLE tasks (
    id                  INTEGER PRIMARY KEY,
    scenario_id         INTEGER NOT NULL REFERENCES scenarios (id),
    scenario_version_id INTEGER NOT NULL REFERENCES scenario_versions (id),
    user_id             INTEGER NOT NULL REFERENCES users (tg_id),
    status              TEXT    NOT NULL CHECK (status IN
        ('created', 'sent', 'accepted', 'declined', 'reported', 'expired', 'reviewed')),
    due_days            INTEGER NOT NULL,          -- срок в днях, отсчёт от принятия
    created_by          INTEGER NOT NULL,
    created_at          INTEGER NOT NULL,
    sent_at             INTEGER NOT NULL DEFAULT 0,
    accepted_at         INTEGER NOT NULL DEFAULT 0,
    due_at              INTEGER NOT NULL DEFAULT 0,
    declined_at         INTEGER NOT NULL DEFAULT 0,
    reported_at         INTEGER NOT NULL DEFAULT 0,
    reviewed_at         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_tasks_user   ON tasks (user_id, status);
CREATE INDEX idx_tasks_status ON tasks (status);
-- Антидубль: у покупателя не больше одного незавершённого задания по одному сценарию.
CREATE UNIQUE INDEX uq_tasks_active ON tasks (user_id, scenario_id)
    WHERE status IN ('created', 'sent', 'accepted', 'expired');

-- История задания: создание, отправка, смена статусов (позже и напоминания).
CREATE TABLE task_events (
    id          INTEGER PRIMARY KEY,
    task_id     INTEGER NOT NULL REFERENCES tasks (id),
    kind        TEXT    NOT NULL,
    from_status TEXT    NOT NULL DEFAULT '',
    to_status   TEXT    NOT NULL DEFAULT '',
    actor_id    INTEGER NOT NULL DEFAULT 0,
    details     TEXT    NOT NULL DEFAULT '',
    at          INTEGER NOT NULL
);
CREATE INDEX idx_task_events_task ON task_events (task_id, id);

-- Состояние пошаговых диалогов (мастер назначения, позже мастер отчёта).
CREATE TABLE fsm_states (
    user_id    INTEGER PRIMARY KEY,
    state      TEXT    NOT NULL,
    data       TEXT    NOT NULL DEFAULT '{}',
    updated_at INTEGER NOT NULL
);
