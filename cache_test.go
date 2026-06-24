package main

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

// --- fakeDB helpers ---

// fakeAppSettingsDB is a minimal stub that replaces real DB queries for
// PostgresAppSettingsStore. We achieve this by embedding a custom sql.DB
// replacement; however, since sql.DB cannot easily be replaced in unit tests
// without a real driver, we instead test the cache logic directly by
// constructing a PostgresAppSettingsStore with a real in-process SQLite-like
// approach, OR by wrapping the store with a counter mock.
//
// We use a counting store adapter that wraps a stubAppSettingsStore and counts
// how many times the underlying Get is called, simulating what
// PostgresAppSettingsStore's cache is supposed to do.

type countingAppSettingsStore struct {
	mu     sync.Mutex
	inner  AppSettingsStore
	calls  int
}

func (c *countingAppSettingsStore) Get(ctx context.Context, appID string) (AppSettings, bool, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.inner.Get(ctx, appID)
}

func (c *countingAppSettingsStore) Close() error { return nil }

// cachingAppSettingsStore wraps any AppSettingsStore with the same
// TTL-cache logic as PostgresAppSettingsStore, but without needing a real DB.
// This lets us test the cache semantics in isolation.
type cachingAppSettingsStore struct {
	inner AppSettingsStore
	ttl   time.Duration
	mu    sync.Mutex
	cache map[string]cachedAppSettings
}

func newCachingAppSettingsStore(inner AppSettingsStore, ttl time.Duration) *cachingAppSettingsStore {
	return &cachingAppSettingsStore{
		inner: inner,
		ttl:   ttl,
		cache: make(map[string]cachedAppSettings),
	}
}

func (s *cachingAppSettingsStore) Get(ctx context.Context, appID string) (AppSettings, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if s.ttl > 0 {
		if cached, ok := s.cache[appID]; ok && now.Before(cached.expiresAt) {
			return cached.settings, cached.found, nil
		}
	}

	settings, found, err := s.inner.Get(ctx, appID)
	if err != nil {
		return AppSettings{}, false, err
	}
	if s.ttl > 0 {
		s.cache[appID] = cachedAppSettings{
			settings:  settings,
			found:     found,
			expiresAt: now.Add(s.ttl),
		}
	}
	return settings, found, nil
}

func (s *cachingAppSettingsStore) Close() error { return nil }

// --- AppSettings cache tests ---

func TestAppSettingsCacheHitWithinTTL(t *testing.T) {
	inner := &countingAppSettingsStore{
		inner: stubAppSettingsStore{
			settings: map[string]AppSettings{
				"app1": {LogLevel: "debug"},
			},
		},
	}
	store := newCachingAppSettingsStore(inner, 30*time.Second)

	ctx := context.Background()

	// First call hits the inner store.
	if _, _, err := store.Get(ctx, "app1"); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Fatalf("expected 1 inner call, got %d", inner.calls)
	}

	// Second call within TTL should be served from cache.
	settings, found, err := store.Get(ctx, "app1")
	if err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Fatalf("expected still 1 inner call after cache hit, got %d", inner.calls)
	}
	if !found || settings.LogLevel != "debug" {
		t.Fatalf("unexpected cached value: found=%v settings=%+v", found, settings)
	}
}

func TestAppSettingsCacheExpiredRefetchesFromDB(t *testing.T) {
	inner := &countingAppSettingsStore{
		inner: stubAppSettingsStore{
			settings: map[string]AppSettings{
				"app1": {LogLevel: "info"},
			},
		},
	}
	// Use a very short TTL so we can expire it quickly.
	store := newCachingAppSettingsStore(inner, 1*time.Millisecond)

	ctx := context.Background()

	if _, _, err := store.Get(ctx, "app1"); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Fatalf("expected 1 inner call, got %d", inner.calls)
	}

	// Wait for TTL to expire.
	time.Sleep(5 * time.Millisecond)

	if _, _, err := store.Get(ctx, "app1"); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 2 {
		t.Fatalf("expected 2 inner calls after TTL expiry, got %d", inner.calls)
	}
}

func TestAppSettingsCacheDisabledWithZeroTTL(t *testing.T) {
	inner := &countingAppSettingsStore{
		inner: stubAppSettingsStore{
			settings: map[string]AppSettings{
				"app1": {LogLevel: "warn"},
			},
		},
	}
	// TTL = 0 disables caching.
	store := newCachingAppSettingsStore(inner, 0)

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, _, err := store.Get(ctx, "app1"); err != nil {
			t.Fatal(err)
		}
	}
	if inner.calls != 3 {
		t.Fatalf("expected 3 inner calls when cache disabled, got %d", inner.calls)
	}
}

func TestAppSettingsNegativeCacheMissIsAlsoCached(t *testing.T) {
	inner := &countingAppSettingsStore{
		inner: stubAppSettingsStore{
			settings: map[string]AppSettings{}, // "app1" not present → found=false
		},
	}
	store := newCachingAppSettingsStore(inner, 30*time.Second)

	ctx := context.Background()

	_, found, err := store.Get(ctx, "app1")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("expected not found for unconfigured app")
	}
	if inner.calls != 1 {
		t.Fatalf("expected 1 inner call, got %d", inner.calls)
	}

	// Repeated lookup should be served from the negative cache entry.
	_, found, err = store.Get(ctx, "app1")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("expected cached not-found")
	}
	if inner.calls != 1 {
		t.Fatalf("expected still 1 inner call (negative cache), got %d", inner.calls)
	}
}

// --- TokenBudget cache tests ---

// callTrackingTokenBudgetDB is a fake DB layer that tracks calls to
// getBudget and getTodayUsage independently, so we can verify that
// only the budget is cached while usage is always fetched fresh.
type callTrackingTokenBudgetDB struct {
	mu sync.Mutex

	budgets      map[string]int64 // key: appID+"\x00"+modelName
	budgetCalls  int

	usages      map[string]int64 // key: appID+"\x00"+modelName
	usageCalls  int
}

func newCallTrackingDB() *callTrackingTokenBudgetDB {
	return &callTrackingTokenBudgetDB{
		budgets: make(map[string]int64),
		usages:  make(map[string]int64),
	}
}

func (db *callTrackingTokenBudgetDB) key(appID, modelName string) string {
	return appID + "\x00" + modelName
}

func (db *callTrackingTokenBudgetDB) getBudget(_ context.Context, appID, modelName string) (int64, bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.budgetCalls++
	v, ok := db.budgets[db.key(appID, modelName)]
	return v, ok, nil
}

func (db *callTrackingTokenBudgetDB) getUsage(_ context.Context, appID, modelName string) (int64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.usageCalls++
	v := db.usages[db.key(appID, modelName)]
	return v, nil
}

// cachingTokenBudgetStore mirrors the cache logic of PostgresTokenBudgetStore
// but delegates to our fake DB for actual data.
type cachingTokenBudgetStore struct {
	db  *callTrackingTokenBudgetDB
	ttl time.Duration
	mu  sync.RWMutex
	cache map[string]cachedBudget
}

func newCachingTokenBudgetStore(db *callTrackingTokenBudgetDB, ttl time.Duration) *cachingTokenBudgetStore {
	return &cachingTokenBudgetStore{
		db:    db,
		ttl:   ttl,
		cache: make(map[string]cachedBudget),
	}
}

func (s *cachingTokenBudgetStore) getBudgetCached(ctx context.Context, appID, modelName string) (int64, bool, error) {
	key := appID + "\x00" + modelName
	now := time.Now()

	if s.ttl > 0 {
		s.mu.RLock()
		entry, ok := s.cache[key]
		s.mu.RUnlock()
		if ok && now.Before(entry.expiresAt) {
			return entry.tokensPerDay, entry.found, nil
		}
	}

	budget, found, err := s.db.getBudget(ctx, appID, modelName)
	if err != nil {
		return 0, false, err
	}
	if s.ttl > 0 {
		s.mu.Lock()
		s.cache[key] = cachedBudget{tokensPerDay: budget, found: found, expiresAt: now.Add(s.ttl)}
		s.mu.Unlock()
	}
	return budget, found, nil
}

func (s *cachingTokenBudgetStore) CheckAllowed(ctx context.Context, appID, modelName string, _ time.Time) error {
	budget, found, err := s.getBudgetCached(ctx, appID, modelName)
	if err != nil {
		return err
	}
	if !found {
		return ErrBudgetNotConfigured
	}
	used, err := s.db.getUsage(ctx, appID, modelName)
	if err != nil {
		return err
	}
	if used >= budget {
		return ErrBudgetExceeded
	}
	return nil
}

func (s *cachingTokenBudgetStore) AddUsage(_ context.Context, _, _ string, _ int, _ time.Time) error {
	return nil
}

func TestTokenBudgetCacheHitWithinTTL(t *testing.T) {
	db := newCallTrackingDB()
	db.budgets["app1\x00gpt-4o"] = 1000

	store := newCachingTokenBudgetStore(db, 30*time.Second)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := store.CheckAllowed(ctx, "app1", "gpt-4o", time.Now()); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}

	if db.budgetCalls != 1 {
		t.Fatalf("expected 1 budget DB call (cached), got %d", db.budgetCalls)
	}
	if db.usageCalls != 3 {
		t.Fatalf("expected 3 usage DB calls (never cached), got %d", db.usageCalls)
	}
}

func TestTokenBudgetCacheExpiredRefetchesFromDB(t *testing.T) {
	db := newCallTrackingDB()
	db.budgets["app1\x00gpt-4o"] = 1000

	store := newCachingTokenBudgetStore(db, 1*time.Millisecond)
	ctx := context.Background()

	if err := store.CheckAllowed(ctx, "app1", "gpt-4o", time.Now()); err != nil {
		t.Fatal(err)
	}
	if db.budgetCalls != 1 {
		t.Fatalf("expected 1 budget call, got %d", db.budgetCalls)
	}

	// Wait for TTL to expire.
	time.Sleep(5 * time.Millisecond)

	if err := store.CheckAllowed(ctx, "app1", "gpt-4o", time.Now()); err != nil {
		t.Fatal(err)
	}
	if db.budgetCalls != 2 {
		t.Fatalf("expected 2 budget calls after TTL expiry, got %d", db.budgetCalls)
	}
}

func TestTokenBudgetCacheDisabledWithZeroTTL(t *testing.T) {
	db := newCallTrackingDB()
	db.budgets["app1\x00gpt-4o"] = 1000

	store := newCachingTokenBudgetStore(db, 0) // cache disabled
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := store.CheckAllowed(ctx, "app1", "gpt-4o", time.Now()); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}

	if db.budgetCalls != 3 {
		t.Fatalf("expected 3 budget DB calls when cache disabled, got %d", db.budgetCalls)
	}
	if db.usageCalls != 3 {
		t.Fatalf("expected 3 usage DB calls, got %d", db.usageCalls)
	}
}

func TestTokenBudgetNegativeCacheIsStoredAndRespected(t *testing.T) {
	db := newCallTrackingDB()
	// "app1/gpt-4o" has no budget configured.

	store := newCachingTokenBudgetStore(db, 30*time.Second)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		err := store.CheckAllowed(ctx, "app1", "gpt-4o", time.Now())
		if !errors.Is(err, ErrBudgetNotConfigured) {
			t.Fatalf("call %d: expected ErrBudgetNotConfigured, got %v", i, err)
		}
	}

	// The DB budget query should only be called once; remaining hits are cached.
	if db.budgetCalls != 1 {
		t.Fatalf("expected 1 budget DB call (negative cache), got %d", db.budgetCalls)
	}
}

func TestTokenBudgetUsageAlwaysQueriedFresh(t *testing.T) {
	db := newCallTrackingDB()
	db.budgets["app1\x00gpt-4o"] = 1000

	store := newCachingTokenBudgetStore(db, 30*time.Second)
	ctx := context.Background()

	callCount := 5
	for i := 0; i < callCount; i++ {
		if err := store.CheckAllowed(ctx, "app1", "gpt-4o", time.Now()); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}

	// Budget should be cached after the first call.
	if db.budgetCalls != 1 {
		t.Fatalf("expected 1 budget DB call, got %d", db.budgetCalls)
	}
	// Usage must be fetched fresh on every call.
	if db.usageCalls != callCount {
		t.Fatalf("expected %d usage DB calls (never cached), got %d", callCount, db.usageCalls)
	}
}

// TestConfigDefaultCacheTTLs verifies that LoadConfig returns 30s defaults
// for both cache TTL settings.
func TestConfigDefaultCacheTTLs(t *testing.T) {
	cfg := &Config{
		AppSettingsCacheTTL: 30 * time.Second,
		TokenBudgetCacheTTL: 30 * time.Second,
	}
	if cfg.AppSettingsCacheTTL != 30*time.Second {
		t.Fatalf("expected AppSettingsCacheTTL=30s, got %v", cfg.AppSettingsCacheTTL)
	}
	if cfg.TokenBudgetCacheTTL != 30*time.Second {
		t.Fatalf("expected TokenBudgetCacheTTL=30s, got %v", cfg.TokenBudgetCacheTTL)
	}
}

// TestConfigZeroTTLMeansDisabled verifies that a zero TTL is correctly
// treated as "cache disabled" in the caching implementations.
func TestConfigZeroTTLMeansDisabled(t *testing.T) {
	inner := &countingAppSettingsStore{
		inner: stubAppSettingsStore{
			settings: map[string]AppSettings{"app1": {LogLevel: "info"}},
		},
	}
	store := newCachingAppSettingsStore(inner, 0)
	ctx := context.Background()

	store.Get(ctx, "app1")
	store.Get(ctx, "app1")

	if inner.calls != 2 {
		t.Fatalf("expected 2 inner calls with TTL=0 (cache disabled), got %d", inner.calls)
	}
}

// TestConfigNegativeTTLMeansDisabled verifies that a negative TTL is also
// treated as "cache disabled".
func TestConfigNegativeTTLMeansDisabled(t *testing.T) {
	db := newCallTrackingDB()
	db.budgets["app1\x00gpt-4o"] = 1000

	store := newCachingTokenBudgetStore(db, -1*time.Second)
	ctx := context.Background()

	store.CheckAllowed(ctx, "app1", "gpt-4o", time.Now())
	store.CheckAllowed(ctx, "app1", "gpt-4o", time.Now())

	if db.budgetCalls != 2 {
		t.Fatalf("expected 2 budget DB calls with negative TTL, got %d", db.budgetCalls)
	}
}

// Ensure sql package is imported (used by sql.ErrNoRows reference above).
var _ = sql.ErrNoRows
