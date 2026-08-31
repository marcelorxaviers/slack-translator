// Package store provides persistence for the Slack translator app.
//
// It uses a single SQLite file to keep four things:
//   - user_prefs:     which language each Slack user wants to read in
//   - translation_cache: text+targetLang -> translated text (avoids re-spending
//     free-tier requests on repeated phrases, e.g. "good morning", "thank you")
//   - exhausted_models: which OpenRouter models hit a rate limit today, and
//     until when they should be skipped (persists across restarts)
//   - usage_stats:    how many successful translations each model has served
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db *sql.DB
}

// Open creates (or reuses) a SQLite database at path and ensures the schema exists.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS user_prefs (
		user_id    TEXT PRIMARY KEY,
		lang       TEXT NOT NULL,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS translation_cache (
		cache_key  TEXT PRIMARY KEY,
		source_text TEXT NOT NULL,
		target_lang TEXT NOT NULL,
		translated TEXT NOT NULL,
		model      TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS exhausted_models (
		model      TEXT PRIMARY KEY,
		until      TIMESTAMP NOT NULL
	);

	CREATE TABLE IF NOT EXISTS usage_stats (
		model      TEXT PRIMARY KEY,
		successes  INTEGER NOT NULL DEFAULT 0,
		failures   INTEGER NOT NULL DEFAULT 0
	);

	CREATE TABLE IF NOT EXISTS messages (
		channel       TEXT NOT NULL,
		ts            TEXT NOT NULL,
		original_text TEXT NOT NULL,
		sender_name   TEXT NOT NULL DEFAULT '',
		created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (channel, ts)
	);
	`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// Migrate: older databases created before sender_name was added.
	cols, err := s.db.Query(`PRAGMA table_info(messages)`)
	if err != nil {
		return err
	}
	defer cols.Close()
	hasSender := false
	for cols.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := cols.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "sender_name" {
			hasSender = true
			break
		}
	}
	if !hasSender {
		if _, err := s.db.Exec(`ALTER TABLE messages ADD COLUMN sender_name TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}

// ---------- User language preferences ----------

// SetUserLang sets the language a given Slack user wants translations in.
func (s *Store) SetUserLang(userID, lang string) error {
	_, err := s.db.Exec(`
		INSERT INTO user_prefs (user_id, lang, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(user_id) DO UPDATE SET lang = excluded.lang, updated_at = CURRENT_TIMESTAMP
	`, userID, lang)
	return err
}

// GetUserLang returns the preferred language for a user, and whether it was set.
func (s *Store) GetUserLang(userID string) (lang string, found bool, err error) {
	row := s.db.QueryRow(`SELECT lang FROM user_prefs WHERE user_id = ?`, userID)
	err = row.Scan(&lang)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return lang, true, nil
}

// ---------- Translation cache ----------

// cacheKey builds a stable key from the source text and target language.
// Hashing avoids issues with very long messages as primary keys.
func cacheKey(sourceText, targetLang string) string {
	h := sha256.Sum256([]byte(targetLang + "\x00" + sourceText))
	return hex.EncodeToString(h[:])
}

// GetCachedTranslation returns a cached translation if we've translated this
// exact text to this language before.
func (s *Store) GetCachedTranslation(sourceText, targetLang string) (translated, model string, found bool, err error) {
	key := cacheKey(sourceText, targetLang)
	row := s.db.QueryRow(`SELECT translated, model FROM translation_cache WHERE cache_key = ?`, key)
	err = row.Scan(&translated, &model)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return translated, model, true, nil
}

// PutCachedTranslation stores a translation result for future reuse.
func (s *Store) PutCachedTranslation(sourceText, targetLang, translated, model string) error {
	key := cacheKey(sourceText, targetLang)
	_, err := s.db.Exec(`
		INSERT INTO translation_cache (cache_key, source_text, target_lang, translated, model, created_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(cache_key) DO UPDATE SET
			translated = excluded.translated,
			model = excluded.model,
			created_at = CURRENT_TIMESTAMP
	`, key, sourceText, targetLang, translated, model)
	return err
}

// ---------- Exhausted models (persisted rate-limit cooldowns) ----------

// MarkModelExhausted records that a model hit a rate limit and should be
// skipped until the given time (typically the next UTC midnight, matching
// OpenRouter's daily free-tier reset).
func (s *Store) MarkModelExhausted(model string, until time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO exhausted_models (model, until)
		VALUES (?, ?)
		ON CONFLICT(model) DO UPDATE SET until = excluded.until
	`, model, until.UTC())
	return err
}

// LoadExhaustedModels returns a map of model -> until-time for all models
// that are still in cooldown right now. Call this on startup to restore
// state after a restart.
func (s *Store) LoadExhaustedModels() (map[string]time.Time, error) {
	rows, err := s.db.Query(`SELECT model, until FROM exhausted_models WHERE until > CURRENT_TIMESTAMP`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]time.Time)
	for rows.Next() {
		var model string
		var until time.Time
		if err := rows.Scan(&model, &until); err != nil {
			return nil, err
		}
		result[model] = until
	}
	return result, rows.Err()
}

// PruneExpiredExhaustions removes cooldown rows that are no longer active.
// Safe to call periodically; purely a housekeeping/cleanliness operation.
func (s *Store) PruneExpiredExhaustions() error {
	_, err := s.db.Exec(`DELETE FROM exhausted_models WHERE until <= CURRENT_TIMESTAMP`)
	return err
}

// ---------- Usage stats ----------

// RecordSuccess increments the success counter for a model.
func (s *Store) RecordSuccess(model string) error {
	_, err := s.db.Exec(`
		INSERT INTO usage_stats (model, successes, failures)
		VALUES (?, 1, 0)
		ON CONFLICT(model) DO UPDATE SET successes = successes + 1
	`, model)
	return err
}

// RecordFailure increments the failure counter for a model.
func (s *Store) RecordFailure(model string) error {
	_, err := s.db.Exec(`
		INSERT INTO usage_stats (model, successes, failures)
		VALUES (?, 0, 1)
		ON CONFLICT(model) DO UPDATE SET failures = failures + 1
	`, model)
	return err
}

// ---------- Original message text (for the "show original" button) ----------

// SaveMessage remembers the original text of a message keyed by channel+ts,
// so a later button click ("View original" / "View in English") can rebuild
// any view of it without relying on Slack API scopes to re-fetch history.
func (s *Store) SaveMessage(channel, ts, originalText, senderName string) error {
	_, err := s.db.Exec(`
		INSERT INTO messages (channel, ts, original_text, sender_name)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(channel, ts) DO UPDATE SET
			original_text = excluded.original_text,
			sender_name = excluded.sender_name
	`, channel, ts, originalText, senderName)
	return err
}

// GetMessage returns the original text and sender name of a previously saved
// message.
func (s *Store) GetMessage(channel, ts string) (originalText, senderName string, found bool, err error) {
	row := s.db.QueryRow(`SELECT original_text, sender_name FROM messages WHERE channel = ? AND ts = ?`, channel, ts)
	err = row.Scan(&originalText, &senderName)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return originalText, senderName, true, nil
}

type ModelUsage struct {
	Model     string
	Successes int
	Failures  int
}

// UsageStats returns per-model success/failure counts, most-used first.
func (s *Store) UsageStats() ([]ModelUsage, error) {
	rows, err := s.db.Query(`SELECT model, successes, failures FROM usage_stats ORDER BY successes DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ModelUsage
	for rows.Next() {
		var u ModelUsage
		if err := rows.Scan(&u.Model, &u.Successes, &u.Failures); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
