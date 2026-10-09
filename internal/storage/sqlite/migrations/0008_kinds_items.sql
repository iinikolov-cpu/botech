-- Типы заданий (покупатель / продавец), пул айтемов для покупателей.
--
-- users.kinds: какие типы заданий пользователь готов делать, через запятую (buyer, seller).
-- Пустая строка значит «ещё не выбрал»: пока не выберет, задание ему выдать нельзя.
ALTER TABLE users ADD COLUMN kinds TEXT NOT NULL DEFAULT '';

-- scenarios.kind: тип сценария. Все прежние сценарии покупательские.
ALTER TABLE scenarios ADD COLUMN kind TEXT NOT NULL DEFAULT 'buyer' CHECK (kind IN ('buyer', 'seller'));

-- Айтемы: товары, из которых покупатель выбирает, что купить. Одноразовые: свободен ->
-- выбран покупателем под задание (reserved) -> куплен после отчёта (used). При отмене
-- задания айтем возвращается в оборот. task_id = 0, пока айтем свободен.
CREATE TABLE items (
    id          INTEGER PRIMARY KEY,
    title       TEXT    NOT NULL,
    url         TEXT    NOT NULL UNIQUE,
    price       INTEGER NOT NULL DEFAULT 0,
    note        TEXT    NOT NULL DEFAULT '',
    added_by    INTEGER NOT NULL,
    created_at  INTEGER NOT NULL,
    status      TEXT    NOT NULL DEFAULT 'free' CHECK (status IN ('free', 'reserved', 'used')),
    task_id     INTEGER NOT NULL DEFAULT 0,
    reserved_at INTEGER NOT NULL DEFAULT 0,
    finished_at INTEGER NOT NULL DEFAULT 0
);
-- Одно задание: не больше одного айтема.
CREATE UNIQUE INDEX uq_items_task ON items (task_id) WHERE task_id > 0;
CREATE INDEX idx_items_status ON items (status, id);
