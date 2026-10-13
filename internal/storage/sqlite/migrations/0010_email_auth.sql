-- Вход по почте сотрудника: привязка почты к пользователю и одноразовые коды из письма.
--
-- users.email: каноническая почта сотрудника (строчными, без +метки). Пусто у пользователей,
-- вошедших по инвайту, и у админов. Одна почта привязывается не больше чем к одному аккаунту.
ALTER TABLE users ADD COLUMN email TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX uq_users_email ON users (email) WHERE email <> '';

-- Выданный код (не больше одного на Telegram-аккаунт). Сам код не хранится, только хэш с солью.
CREATE TABLE email_otps (
    tg_id      INTEGER PRIMARY KEY,
    email      TEXT    NOT NULL,
    salt       TEXT    NOT NULL,
    code_hash  TEXT    NOT NULL,
    expires_at INTEGER NOT NULL,
    attempts   INTEGER NOT NULL DEFAULT 0,
    sent_at    INTEGER NOT NULL
);

-- Журнал отправок для ограничения частоты (по аккаунту, по почте и общий).
CREATE TABLE email_otp_sends (
    id    INTEGER PRIMARY KEY,
    tg_id INTEGER NOT NULL,
    email TEXT    NOT NULL,
    at    INTEGER NOT NULL
);
CREATE INDEX idx_otp_sends_tg    ON email_otp_sends (tg_id, at);
CREATE INDEX idx_otp_sends_email ON email_otp_sends (email, at);
CREATE INDEX idx_otp_sends_at    ON email_otp_sends (at);
