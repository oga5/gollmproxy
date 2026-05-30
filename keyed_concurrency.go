package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	concurrencyScopeApp      = "app"
	concurrencyScopeModel    = "model"
	concurrencyScopeAppModel = "app_model"
)

var (
	ErrConcurrencyQueueFull      = errors.New("concurrency wait queue is full")
	ErrConcurrencyWaitTimeout    = errors.New("timed out while waiting for concurrency slot")
	ErrConcurrencyCanceled       = errors.New("request canceled while waiting for concurrency slot")
	ErrConcurrencyAppIDRequired  = errors.New("app_id is required for concurrency control")
	ErrConcurrencyModelRequired  = errors.New("model_name is required for concurrency control")
	ErrConcurrencyInvalidKey     = errors.New("concurrency key is required")
	ErrConcurrencyInvalidSetting = errors.New("invalid concurrency control setting")
)

type admissionDecision struct {
	Key                 string
	Waited              bool
	WaitDuration        time.Duration
	QueueResult         string
	RejectReason        string
	InflightAtAdmission int
	WaitingAtAdmission  int
}

type admissionHandle struct {
	controller *KeyedConcurrencyController
	key        string
	once       sync.Once
}

func (h *admissionHandle) Release() {
	if h == nil || h.controller == nil || h.key == "" {
		return
	}
	h.once.Do(func() {
		h.controller.release(h.key)
	})
}

type keyedConcurrencyState struct {
	inflight int
	waiting  int
	waiters  []chan struct{}
}

type KeyedConcurrencyController struct {
	mu             sync.Mutex
	maxConcurrency int
	maxQueue       int
	perKey         map[string]*keyedConcurrencyState
}

func NewKeyedConcurrencyController(maxConcurrency, maxQueue int) (*KeyedConcurrencyController, error) {
	if maxConcurrency <= 0 {
		return nil, ErrConcurrencyInvalidSetting
	}
	if maxQueue < 0 {
		return nil, ErrConcurrencyInvalidSetting
	}
	return &KeyedConcurrencyController{
		maxConcurrency: maxConcurrency,
		maxQueue:       maxQueue,
		perKey:         make(map[string]*keyedConcurrencyState),
	}, nil
}

func (c *KeyedConcurrencyController) Acquire(ctx context.Context, key string, maxWait time.Duration) (*admissionHandle, admissionDecision, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, admissionDecision{}, ErrConcurrencyInvalidKey
	}

	decision := admissionDecision{Key: key}

	c.mu.Lock()
	st := c.perKey[key]
	if st == nil {
		st = &keyedConcurrencyState{}
		c.perKey[key] = st
	}

	decision.InflightAtAdmission = st.inflight
	decision.WaitingAtAdmission = st.waiting

	if st.inflight < c.maxConcurrency {
		st.inflight++
		c.mu.Unlock()
		decision.QueueResult = "acquired"
		return &admissionHandle{controller: c, key: key}, decision, nil
	}
	if st.waiting >= c.maxQueue {
		c.mu.Unlock()
		decision.QueueResult = "rejected"
		decision.RejectReason = "queue_full"
		return nil, decision, ErrConcurrencyQueueFull
	}

	waiter := make(chan struct{})
	st.waiting++
	st.waiters = append(st.waiters, waiter)
	c.mu.Unlock()

	waitStarted := time.Now()
	decision.Waited = true

	var timer <-chan time.Time
	if maxWait > 0 {
		t := time.NewTimer(maxWait)
		defer t.Stop()
		timer = t.C
	}

	select {
	case <-waiter:
		decision.WaitDuration = time.Since(waitStarted)
		decision.QueueResult = "acquired"
		return &admissionHandle{controller: c, key: key}, decision, nil
	case <-ctx.Done():
		decision.WaitDuration = time.Since(waitStarted)
		if c.removeWaiter(key, waiter) {
			decision.QueueResult = "rejected"
			decision.RejectReason = "context_canceled"
			return nil, decision, ErrConcurrencyCanceled
		}
		decision.QueueResult = "acquired"
		return &admissionHandle{controller: c, key: key}, decision, nil
	case <-timer:
		decision.WaitDuration = time.Since(waitStarted)
		if c.removeWaiter(key, waiter) {
			decision.QueueResult = "rejected"
			decision.RejectReason = "wait_timeout"
			return nil, decision, ErrConcurrencyWaitTimeout
		}
		decision.QueueResult = "acquired"
		return &admissionHandle{controller: c, key: key}, decision, nil
	}
}

func (c *KeyedConcurrencyController) removeWaiter(key string, waiter chan struct{}) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	st := c.perKey[key]
	if st == nil {
		return false
	}

	for i, queued := range st.waiters {
		if queued != waiter {
			continue
		}
		st.waiters = append(st.waiters[:i], st.waiters[i+1:]...)
		st.waiting--
		if st.waiting < 0 {
			st.waiting = 0
		}
		if st.inflight == 0 && st.waiting == 0 {
			delete(c.perKey, key)
		}
		return true
	}
	return false
}

func (c *KeyedConcurrencyController) release(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	st := c.perKey[key]
	if st == nil {
		return
	}

	if st.inflight > 0 {
		st.inflight--
	}

	if len(st.waiters) > 0 {
		waiter := st.waiters[0]
		st.waiters = st.waiters[1:]
		if st.waiting > 0 {
			st.waiting--
		}
		st.inflight++
		close(waiter)
	}

	if st.inflight == 0 && st.waiting == 0 {
		delete(c.perKey, key)
	}
}

func normalizeConcurrencyScope(scope string) string {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case concurrencyScopeApp, "app_id":
		return concurrencyScopeApp
	case concurrencyScopeModel, "model_name":
		return concurrencyScopeModel
	case concurrencyScopeAppModel, "app+model", "app_model_name", "app_id+model_name":
		return concurrencyScopeAppModel
	default:
		return concurrencyScopeAppModel
	}
}

func resolveConcurrencyKey(scope string, metadata map[string]any, modelName string) (string, error) {
	scope = normalizeConcurrencyScope(scope)

	appID, _ := metadataStringValue(metadata, "app_id")
	modelName = strings.TrimSpace(modelName)

	switch scope {
	case concurrencyScopeApp:
		if appID == "" {
			return "", ErrConcurrencyAppIDRequired
		}
		return appID, nil
	case concurrencyScopeModel:
		if modelName == "" {
			return "", ErrConcurrencyModelRequired
		}
		return modelName, nil
	default:
		if appID == "" {
			return "", ErrConcurrencyAppIDRequired
		}
		if modelName == "" {
			return "", ErrConcurrencyModelRequired
		}
		return appID + ":" + modelName, nil
	}
}
