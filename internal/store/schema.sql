-- Append-only session event log of Barroso Harness.
--
-- Three tables and one obligation: `events` is append-only. Nothing here is
-- edited or deleted during a session's life — the transcript history is
-- rebuilt from `seq` order, and the log's reproducibility depends on it.
--
-- `sessions` is the only mutable row: `updated_at` (TouchSession), `title`
-- (SetTitle) and `metadata_json`. These are index metadata, not content.
--
-- `blobs` holds what does not (and should not) fit in the log: large tool
-- outputs. The event references them by {"blob": "<sha256hex>", "size": N},
-- so the log stays short and readable without losing data.
--
-- ONE FILE PER SESSION. Each conversation lives in its own database, so two
-- `bh -acp` processes writing at the same time contend on different files: a
-- lock in one session no longer stops the others from persisting. `blobs` is
-- gone from here: the content is a file named by its sha256, under the store's
-- `blobs` directory.

CREATE TABLE IF NOT EXISTS sessions (
    id             TEXT PRIMARY KEY,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    title          TEXT NOT NULL DEFAULT '',
    metadata_json  TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS events (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id    TEXT NOT NULL REFERENCES sessions(id),
    ts            TEXT NOT NULL,
    kind          TEXT NOT NULL,
    payload_json  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_session_seq
    ON events(session_id, seq);

-- Bookkeeping of the legacy migration (only present in files that were copied
-- from the old shared sessions.db): the highest legacy seq already copied and
-- the title as the legacy database had it. They let a later open pick up
-- events an older `bh` process wrote to the legacy database after the copy.
CREATE TABLE IF NOT EXISTS meta (
    key    TEXT PRIMARY KEY,
    value  TEXT NOT NULL
);
