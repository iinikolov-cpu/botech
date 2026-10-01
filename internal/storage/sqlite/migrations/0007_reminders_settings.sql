-- Настройки приложения, которые админ меняет из бота (интервалы напоминаний, тихие часы, время бэкапа).
CREATE TABLE settings (
    key        TEXT    PRIMARY KEY,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
);

-- Журнал отправленных напоминаний. Состояние хранится в БД, поэтому после перезапуска
-- бот не повторяет уже отправленное. base_at это момент, от которого считаются интервалы
-- (отправка задания, принятие, возврат на доработку): новый возврат на доработку начинает
-- отсчёт заново. seq = 0 означает эскалацию админу, seq >= 1 номер напоминания покупателю.
-- skipped = 1: напоминание пропущено (бот долго не работал, отправлено только последнее из просроченных).
CREATE TABLE task_reminders (
    id      INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks (id),
    kind    TEXT    NOT NULL,
    base_at INTEGER NOT NULL,
    seq     INTEGER NOT NULL,
    sent_at INTEGER NOT NULL,
    skipped INTEGER NOT NULL DEFAULT 0,
    UNIQUE (task_id, kind, base_at, seq)
);
