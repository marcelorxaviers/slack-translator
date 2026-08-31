package translator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrAllModelsExhausted is returned when every model in the pool is
// currently in cooldown (rate limited) and there is nothing left to try.
var ErrAllModelsExhausted = errors.New("all translation models are rate limited right now")

// Persistence is the subset of storage the pool needs to survive restarts
// without forgetting which models are in cooldown. store.Store satisfies
// this interface; it's defined here (not imported from store) to avoid a
// circular dependency between the two packages.
type Persistence interface {
	MarkModelExhausted(model string, until time.Time) error
	LoadExhaustedModels() (map[string]time.Time, error)
	RecordSuccess(model string) error
	RecordFailure(model string) error
	GetCachedTranslation(sourceText, targetLang string) (translated, model string, found bool, err error)
	PutCachedTranslation(sourceText, targetLang, translated, model string) error
}

// Result is what callers get back from a successful translation: the text
// and which model actually produced it, so the caller can show that in the
// Slack message (e.g. "via llama-3.3-70b-instruct").
type Result struct {
	Text      string
	Model     string
	FromCache bool
}

// Pool tries a list of OpenRouter models in order, skipping any that are
// currently in cooldown (rate limited), and marks a model as exhausted for
// the rest of the day the moment it reports a 429. This is the automatic
// "exception handling as model switching" behavior requested: callers never
// see a rate limit error unless every configured model is exhausted.
type Pool struct {
	client *Client
	models []string
	store  Persistence

	mu        sync.Mutex
	exhausted map[string]time.Time // model -> cooldown expiry, mirrored from store
}

// NewPool builds a pool over the given models, in priority order (first
// model is tried first). It loads any persisted cooldowns from store so a
// restart doesn't forget that a model was rate limited earlier today.
func NewPool(client *Client, models []string, store Persistence) (*Pool, error) {
	p := &Pool{
		client:    client,
		models:    models,
		store:     store,
		exhausted: make(map[string]time.Time),
	}

	loaded, err := store.LoadExhaustedModels()
	if err != nil {
		return nil, fmt.Errorf("load exhausted models: %w", err)
	}
	p.exhausted = loaded

	return p, nil
}

// availableModels returns configured models that are not currently in
// cooldown, in priority order.
func (p *Pool) availableModels() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	var out []string
	for _, m := range p.models {
		if until, ok := p.exhausted[m]; ok && now.Before(until) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// nextDailyReset returns the next UTC midnight, matching OpenRouter's daily
// free-tier quota reset.
func nextDailyReset() time.Time {
	now := time.Now().UTC()
	tomorrow := now.Truncate(24 * time.Hour).Add(24 * time.Hour)
	return tomorrow
}

func (p *Pool) markExhausted(model string) {
	until := nextDailyReset()

	p.mu.Lock()
	p.exhausted[model] = until
	p.mu.Unlock()

	// Persist so a restart later today still remembers this model is out.
	// Best-effort: if persistence fails we still have the in-memory cooldown.
	_ = p.store.MarkModelExhausted(model, until)
}

// Translate returns a cached result if we've translated this exact text to
// this language before; otherwise it walks the model list in order,
// automatically skipping/marking exhausted models on rate limit errors,
// until one succeeds or all are exhausted.
func (p *Pool) Translate(ctx context.Context, text, targetLangCode, targetLangName string) (*Result, error) {
	if cached, model, found, err := p.store.GetCachedTranslation(text, targetLangCode); err == nil && found {
		return &Result{Text: cached, Model: model, FromCache: true}, nil
	}

	candidates := p.availableModels()
	if len(candidates) == 0 {
		return nil, ErrAllModelsExhausted
	}

	var lastErr error
	for _, model := range candidates {
		translated, err := p.client.Translate(ctx, model, text, targetLangName)
		if err == nil {
			_ = p.store.RecordSuccess(model)
			_ = p.store.PutCachedTranslation(text, targetLangCode, translated, model)
			return &Result{Text: translated, Model: model}, nil
		}

		if errors.Is(err, ErrRateLimit) || errors.Is(err, ErrModelUnavailable) {
			_ = p.store.RecordFailure(model)
			p.markExhausted(model)
			lastErr = fmt.Errorf("%s: %w", model, err)
			continue // automatic fallback to the next model
		}

		// Non-rate-limit error (timeout, 5xx, empty response, etc): this
		// model isn't necessarily exhausted, but this attempt failed.
		// We still try the next model rather than failing the whole
		// translation outright, since a transient error on one model
		// shouldn't block the message from being translated.
		_ = p.store.RecordFailure(model)
		lastErr = fmt.Errorf("%s: %w", model, err)
		continue
	}

	if lastErr == nil {
		lastErr = ErrAllModelsExhausted
	}
	return nil, lastErr
}
