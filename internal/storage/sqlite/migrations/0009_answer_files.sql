-- Несколько фото или видео в одном ответе отчёта. Список файлов хранится JSON-ом
-- ([{"id":...,"uid":...}, ...]); пусто означает «один файл из file_id / file_unique_id» (старые отчёты).
-- Колонки file_id и file_unique_id по-прежнему содержат первый файл.
ALTER TABLE report_answers ADD COLUMN files TEXT NOT NULL DEFAULT '';
