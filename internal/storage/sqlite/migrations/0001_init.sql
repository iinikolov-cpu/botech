-- Пользователи: админы и тайные покупатели.
CREATE TABLE users (
    tg_id       INTEGER PRIMARY KEY,
    role        TEXT    NOT NULL DEFAULT 'buyer'   CHECK (role IN ('admin', 'buyer')),
    status      TEXT    NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'active', 'blocked')),
    lang        TEXT    NOT NULL DEFAULT 'ru',
    first_name  TEXT    NOT NULL DEFAULT '',
    username    TEXT    NOT NULL DEFAULT '',
    invite_code TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX idx_users_role_status ON users (role, status);

-- Инвайты. expires_at = 0 означает «бессрочно».
CREATE TABLE invites (
    code       TEXT    PRIMARY KEY,
    created_by INTEGER NOT NULL,
    max_uses   INTEGER NOT NULL,
    used_count INTEGER NOT NULL DEFAULT 0,
    expires_at INTEGER NOT NULL DEFAULT 0,
    revoked    INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);

-- Журнал действий админов.
CREATE TABLE admin_audit (
    id        INTEGER PRIMARY KEY,
    admin_id  INTEGER NOT NULL,
    action    TEXT    NOT NULL,
    entity    TEXT    NOT NULL DEFAULT '',
    entity_id TEXT    NOT NULL DEFAULT '',
    details   TEXT    NOT NULL DEFAULT '',
    at        INTEGER NOT NULL
);
CREATE INDEX idx_admin_audit_at ON admin_audit (at DESC);
