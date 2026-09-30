-- Промокоды с несколькими использованиями.
-- Код многоразовый: used_count растёт при каждом выполненном задании, а пока код закреплён
-- за активным заданием (active_task_id != 0), другим заданиям он не выдаётся.
-- Лимит использований общий и задаётся настройкой приложения, поэтому в таблице его нет.
CREATE TABLE promo_codes_new (
    id             INTEGER PRIMARY KEY,
    code           TEXT    NOT NULL UNIQUE,
    used_count     INTEGER NOT NULL DEFAULT 0,
    active_task_id INTEGER NOT NULL DEFAULT 0,
    added_by       INTEGER NOT NULL,
    created_at     INTEGER NOT NULL
);

-- История выдач: какому заданию и кому был выдан код и чем закончилось.
-- Текст кода хранится копией, чтобы история пережила удаление кода из пула.
CREATE TABLE promo_assignments (
    id          INTEGER PRIMARY KEY,
    promo_id    INTEGER NOT NULL,
    code        TEXT    NOT NULL,
    task_id     INTEGER NOT NULL,
    user_id     INTEGER NOT NULL,
    issued_at   INTEGER NOT NULL,
    finished_at INTEGER NOT NULL DEFAULT 0,
    outcome     TEXT    NOT NULL DEFAULT 'active' CHECK (outcome IN ('active', 'used', 'released'))
);
CREATE UNIQUE INDEX uq_promo_assignment_task ON promo_assignments (task_id);

-- Перенос данных из прежней схемы (свободный / выдан / использован).
INSERT INTO promo_codes_new (id, code, used_count, active_task_id, added_by, created_at)
SELECT id, code,
       CASE WHEN status = 'used'   THEN 1       ELSE 0 END,
       CASE WHEN status = 'issued' THEN task_id ELSE 0 END,
       added_by, created_at
  FROM promo_codes;

INSERT INTO promo_assignments (promo_id, code, task_id, user_id, issued_at, finished_at, outcome)
SELECT id, code, task_id, user_id, issued_at,
       CASE WHEN status = 'used' THEN used_at ELSE 0 END,
       CASE WHEN status = 'used' THEN 'used' ELSE 'active' END
  FROM promo_codes
 WHERE task_id != 0;

DROP TABLE promo_codes;
ALTER TABLE promo_codes_new RENAME TO promo_codes;
CREATE INDEX idx_promo_active ON promo_codes (active_task_id, used_count);
