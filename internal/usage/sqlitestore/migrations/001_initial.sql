CREATE TABLE anthropic_turn (
  id                          INTEGER PRIMARY KEY,
  at_ms                       INTEGER NOT NULL,
  at_utc                      TEXT GENERATED ALWAYS AS (
                                strftime('%Y-%m-%dT%H:%M:%fZ', at_ms/1000.0, 'unixepoch')
                              ) VIRTUAL,
  request_id                  TEXT    NOT NULL,
  message_id                  TEXT    NOT NULL,
  turn_index                  INTEGER NOT NULL,
  model                       TEXT    NOT NULL,
  transport                   TEXT    NOT NULL CHECK (transport IN ('buffered','sse')),
  input_tokens                INTEGER NOT NULL,  -- UNCACHED REMAINDER
  output_tokens               INTEGER NOT NULL,
  cache_creation_input_tokens INTEGER,           -- additive to input_tokens
  cache_read_input_tokens     INTEGER,           -- additive to input_tokens
  ephemeral_5m_input_tokens   INTEGER,           -- subset of cache_creation_input_tokens
  ephemeral_1h_input_tokens   INTEGER,           -- subset of cache_creation_input_tokens
  thinking_tokens             INTEGER            -- subset of output_tokens; re-tokenized
) STRICT;
CREATE INDEX anthropic_turn_at ON anthropic_turn(at_ms);

CREATE TABLE openai_turn (
  id                 INTEGER PRIMARY KEY,
  at_ms              INTEGER NOT NULL,
  at_utc              TEXT GENERATED ALWAYS AS (
                       strftime('%Y-%m-%dT%H:%M:%fZ', at_ms/1000.0, 'unixepoch')
                     ) VIRTUAL,
  request_id         TEXT    NOT NULL,
  response_id        TEXT    NOT NULL,
  turn_index         INTEGER NOT NULL,
  model              TEXT    NOT NULL,
  transport          TEXT    NOT NULL CHECK (transport IN ('buffered','sse','websocket')),
  input_tokens       INTEGER NOT NULL,  -- COMPLETE count
  cached_tokens      INTEGER,           -- subset of input_tokens
  cache_write_tokens INTEGER,           -- subset of input_tokens
  output_tokens      INTEGER NOT NULL,  -- COMPLETE count
  reasoning_tokens   INTEGER,           -- subset of output_tokens
  total_tokens       INTEGER
) STRICT;
CREATE INDEX openai_turn_at ON openai_turn(at_ms);
