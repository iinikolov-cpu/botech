-- Пул промокодов на оплату доставки.
-- task_id = 0 означает «не привязан». Уникальный индекс гарантирует один код на задание,
-- а UNIQUE(code) не даёт загрузить один код дважды.
CREATE TABLE promo_codes (
    id         INTEGER PRIMARY KEY,
    code       TEXT    NOT NULL UNIQUE,
    status     TEXT    NOT NULL DEFAULT 'free' CHECK (status IN ('free', 'issued', 'used')),
    task_id    INTEGER NOT NULL DEFAULT 0,
    user_id    INTEGER NOT NULL DEFAULT 0,
    added_by   INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    issued_at  INTEGER NOT NULL DEFAULT 0,
    used_at    INTEGER NOT NULL DEFAULT 0,
    used_by    INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX uq_promo_task ON promo_codes (task_id) WHERE task_id != 0;
CREATE INDEX idx_promo_status ON promo_codes (status, id);

-- Отчёты: один на задание.
CREATE TABLE reports (
    id           INTEGER PRIMARY KEY,
    task_id      INTEGER NOT NULL UNIQUE REFERENCES tasks (id),
    user_id      INTEGER NOT NULL,
    late         INTEGER NOT NULL DEFAULT 0,
    submitted_at INTEGER NOT NULL
);

-- Ответы на вопросы чек-листа. Фото и видео хранятся как file_id Telegram, файлов на диске нет.
CREATE TABLE report_answers (
    id             INTEGER PRIMARY KEY,
    report_id      INTEGER NOT NULL REFERENCES reports (id),
    question_key   TEXT    NOT NULL,
    type           TEXT    NOT NULL,
    value          TEXT    NOT NULL DEFAULT '',
    file_id        TEXT    NOT NULL DEFAULT '',
    file_unique_id TEXT    NOT NULL DEFAULT '',
    skipped        INTEGER NOT NULL DEFAULT 0,
    UNIQUE (report_id, question_key)
);

-- Компенсация стоимости товара: только сумма и фото чека, без платёжных реквизитов.
CREATE TABLE compensations (
    id                INTEGER PRIMARY KEY,
    task_id           INTEGER NOT NULL UNIQUE REFERENCES tasks (id),
    user_id           INTEGER NOT NULL,
    amount            INTEGER NOT NULL,
    receipt_file_id   TEXT    NOT NULL,
    receipt_unique_id TEXT    NOT NULL,
    status            TEXT    NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'paid')),
    created_at        INTEGER NOT NULL,
    paid_at           INTEGER NOT NULL DEFAULT 0,
    paid_by           INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_comp_status ON compensations (status, id);
CREATE INDEX idx_comp_receipt ON compensations (receipt_unique_id);
