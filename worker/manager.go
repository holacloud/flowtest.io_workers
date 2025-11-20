package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrWorkerNotFound indicates that the requested worker is not registered.
	ErrWorkerNotFound = errors.New("worker not found")
	// ErrRequestCancelled is returned when the request context is cancelled before completion.
	ErrRequestCancelled = errors.New("request cancelled")
)

// Manager keeps track of connected workers and coordinates proxy requests.
type Manager struct {
	mu      sync.RWMutex
	workers map[string]*Worker
	bySuite map[string]map[string]*workerPool
}

type workerPool struct {
	workers []*Worker
	next    int
}

const (
	workerHeartbeatInterval = 10 * time.Second
	workerStaleAfter        = 3 * workerHeartbeatInterval
)

// WorkerInfo describes a connected worker for listing purposes.
type WorkerInfo struct {
	ID          string    `json:"id"`
	SuiteID     string    `json:"suite_id"`
	Name        string    `json:"name,omitempty"`
	ConnectedAt time.Time `json:"connected_at"`
	LastSeen    time.Time `json:"last_seen"`
	Pending     int       `json:"pending"`
}

// ProxyRequest represents an HTTP request that must be executed by a remote worker.
type ProxyRequest struct {
	ID     string      `json:"id"`
	Method string      `json:"method"`
	URL    string      `json:"url"`
	Header http.Header `json:"header"`
	Host   string      `json:"host,omitempty"`
	Body   []byte      `json:"body,omitempty"`
}

// ProxyResponse is the remote worker response for a proxied request.
type ProxyResponse struct {
	RequestID string      `json:"request_id"`
	Status    int         `json:"status"`
	Header    http.Header `json:"header"`
	Body      []byte      `json:"body,omitempty"`
	Error     string      `json:"error,omitempty"`

	WorkerName string `json:"worker_name,omitempty"`
}

// Worker maintains the queue of pending jobs for a connected agent.
type Worker struct {
	ID          string
	SuiteID     string
	Name        string
	connectedAt time.Time

	mu       sync.Mutex
	lastSeen time.Time
	jobs     chan *job
	pending  map[string]*job
}

type job struct {
	request   *ProxyRequest
	responseC chan *ProxyResponse
}

// NewManager creates an empty worker manager.
func NewManager() *Manager {
	return &Manager{
		workers: map[string]*Worker{},
		bySuite: map[string]map[string]*workerPool{},
	}
}

// Register creates a new worker for the provided suite identifier.
func (m *Manager) Register(suiteID, name string) *Worker {
	workerName := strings.TrimSpace(name)
	if workerName == "" {
		workerName = "Worker"
	}
	worker := &Worker{
		ID:          uuid.NewString(),
		SuiteID:     suiteID,
		Name:        workerName,
		connectedAt: time.Now(),
		lastSeen:    time.Now(),
		jobs:        make(chan *job, 32),
		pending:     map[string]*job{},
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.workers[worker.ID] = worker
	pools, ok := m.bySuite[worker.SuiteID]
	if !ok {
		pools = map[string]*workerPool{}
		m.bySuite[worker.SuiteID] = pools
	}
	pool, ok := pools[worker.Name]
	if !ok {
		pool = &workerPool{}
		pools[worker.Name] = pool
	}
	pool.workers = append(pool.workers, worker)

	return worker
}

// List returns information about all currently registered workers.
func (m *Manager) List() []WorkerInfo {
	now := time.Now()

	m.mu.Lock()
	m.pruneLocked(now)
	workers := make([]*Worker, 0, len(m.workers))
	for _, w := range m.workers {
		workers = append(workers, w)
	}
	m.mu.Unlock()

	infos := make([]WorkerInfo, 0, len(workers))
	for _, w := range workers {
		info := WorkerInfo{ID: w.ID, SuiteID: w.SuiteID, Name: w.Name, ConnectedAt: w.connectedAt}
		w.mu.Lock()
		info.LastSeen = w.lastSeen
		info.Pending = len(w.pending)
		w.mu.Unlock()
		infos = append(infos, info)
	}

	return infos
}

// Client builds an HTTP client that routes requests through the given worker.
func (m *Manager) Client(workerID string) (*http.Client, error) {
	if _, err := m.getWorker(workerID); err != nil {
		return nil, err
	}

	transport := &proxyTransport{manager: m, workerID: workerID}

	return &http.Client{
		Transport:     transport,
		Timeout:       3 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// ClientForName builds an HTTP client that routes requests through any worker matching the provided suite and name.
func (m *Manager) ClientForName(suiteID, workerName string) (*http.Client, error) {
	name := strings.TrimSpace(workerName)
	if name == "" {
		return nil, ErrWorkerNotFound
	}

	if !m.hasWorker(suiteID, name) && !m.hasWorker("*", name) {
		return nil, ErrWorkerNotFound
	}

	transport := &proxyTransport{manager: m, suiteID: suiteID, workerName: name}

	return &http.Client{
		Transport:     transport,
		Timeout:       3 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// getWorker returns the worker identified by workerID.
func (m *Manager) getWorker(workerID string) (*Worker, error) {
	m.mu.RLock()
	worker, ok := m.workers[workerID]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrWorkerNotFound
	}
	if worker.isStale(time.Now()) {
		m.mu.Lock()
		if w, exists := m.workers[workerID]; exists && w.isStale(time.Now()) {
			m.removeWorkerLocked(workerID)
		}
		m.mu.Unlock()
		return nil, ErrWorkerNotFound
	}
	return worker, nil
}

// Pull returns the next request for the worker, blocking until one is available
// or the provided context is cancelled.
func (m *Manager) Pull(ctx context.Context, workerID string) (*ProxyRequest, error) {
	worker, err := m.getWorker(workerID)
	if err != nil {
		return nil, err
	}

	worker.touch()

	ticker := time.NewTicker(workerHeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			worker.touch()
			return nil, ctx.Err()
		case <-ticker.C:
			worker.touch()
		case job := <-worker.jobs:
			worker.touch()
			worker.registerPending(job)
			return job.request, nil
		}
	}
}

// Submit finishes a pending request with the provided response.
func (m *Manager) Submit(ctx context.Context, workerID string, response *ProxyResponse) error {
	worker, err := m.getWorker(workerID)
	if err != nil {
		return err
	}

	worker.touch()
	worker.mu.Lock()
	job := worker.pending[response.RequestID]
	if job != nil {
		delete(worker.pending, response.RequestID)
	}
	worker.mu.Unlock()

	if job == nil {
		return nil
	}

	response.WorkerName = worker.Name

	select {
	case job.responseC <- response:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// dispatch sends the request to the worker and waits for the response.
func (m *Manager) dispatch(ctx context.Context, workerID string, request *ProxyRequest) (*ProxyResponse, error) {
	worker, err := m.getWorker(workerID)
	if err != nil {
		return nil, err
	}

	request.ID = uuid.NewString()
	job := &job{
		request:   request,
		responseC: make(chan *ProxyResponse, 1),
	}

	select {
	case worker.jobs <- job:
	case <-ctx.Done():
		return nil, ErrRequestCancelled
	}

	select {
	case <-ctx.Done():
		worker.unregisterPending(request.ID)
		return nil, ErrRequestCancelled
	case resp := <-job.responseC:
		return resp, nil
	}
}

func (m *Manager) dispatchByName(ctx context.Context, suiteID, workerName string, request *ProxyRequest) (*ProxyResponse, error) {
	name := strings.TrimSpace(workerName)
	if name == "" {
		return nil, ErrWorkerNotFound
	}

	tried := map[string]struct{}{}
	for {
		worker := m.nextWorker(suiteID, name)
		if worker == nil {
			return nil, ErrWorkerNotFound
		}
		if _, seen := tried[worker.ID]; seen {
			return nil, ErrWorkerNotFound
		}
		tried[worker.ID] = struct{}{}
		resp, err := m.dispatch(ctx, worker.ID, request)
		if err == nil {
			return resp, nil
		}
		if errors.Is(err, ErrWorkerNotFound) {
			continue
		}
		return resp, err
	}
}

func (m *Manager) hasWorker(suiteID, name string) bool {
	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	m.pruneLocked(now)

	pools, ok := m.bySuite[suiteID]
	if !ok {
		return false
	}
	pool, ok := pools[name]
	return ok && len(pool.workers) > 0
}

func (m *Manager) nextWorker(suiteID, name string) *Worker {
	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	m.pruneLocked(now)

	pools := map[string]*workerPool{}
	for k, v := range m.bySuite[suiteID] {
		pools[k] = v
	}
	for k, v := range m.bySuite["*"] {
		pools[k] = v
	}

	if len(pools) == 0 {
		return nil
	}

	pool := pools[name]
	if pool == nil || len(pool.workers) == 0 {
		return nil
	}

	checked := 0
	for len(pool.workers) > 0 && checked < len(pool.workers) {
		idx := pool.next % len(pool.workers)
		worker := pool.workers[idx]
		pool.next = (idx + 1) % len(pool.workers)
		checked++
		if worker.isStale(now) {
			m.removeWorkerLocked(worker.ID)
			checked = 0
			continue
		}
		return worker
	}

	return nil
}

func (m *Manager) pruneLocked(now time.Time) {
	for id, worker := range m.workers {
		if worker.isStale(now) {
			m.removeWorkerLocked(id)
		}
	}
}

func (m *Manager) removeWorkerLocked(workerID string) {
	worker, ok := m.workers[workerID]
	if !ok {
		return
	}

	delete(m.workers, workerID)

	pools := m.bySuite[worker.SuiteID]
	if pools == nil {
		return
	}

	pool := pools[worker.Name]
	if pool == nil {
		return
	}

	for i, candidate := range pool.workers {
		if candidate.ID != workerID {
			continue
		}
		pool.workers = append(pool.workers[:i], pool.workers[i+1:]...)
		if len(pool.workers) == 0 {
			delete(pools, worker.Name)
			if len(pools) == 0 {
				delete(m.bySuite, worker.SuiteID)
			}
		} else {
			if pool.next > i {
				pool.next--
			}
			if pool.next >= len(pool.workers) {
				pool.next = 0
			}
		}
		break
	}
}

// proxyTransport routes HTTP requests through a remote worker.
type proxyTransport struct {
	manager    *Manager
	workerID   string
	suiteID    string
	workerName string
}

func (t *proxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body := []byte{}
	if req.Body != nil {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = data
	}

	request := &ProxyRequest{
		Method: req.Method,
		URL:    req.URL.String(),
		Header: req.Header.Clone(),
		Host:   req.Host,
		Body:   body,
	}

	resp, err := t.dispatch(req.Context(), request)
	if err != nil {
		return nil, err
	}

	if resp == nil {
		return nil, errors.New("empty response")
	}

	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}

	response := &http.Response{
		StatusCode: resp.Status,
		Status:     fmt.Sprintf("%d %s", resp.Status, http.StatusText(resp.Status)),
		Header:     resp.Header.Clone(),
		Body:       io.NopCloser(bytes.NewReader(resp.Body)),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Request:    req,
	}
	response.ContentLength = int64(len(resp.Body))

	return response, nil
}

func (t *proxyTransport) dispatch(ctx context.Context, request *ProxyRequest) (*ProxyResponse, error) {
	switch {
	case t.workerID != "":
		return t.manager.dispatch(ctx, t.workerID, request)
	case t.workerName != "":
		return t.manager.dispatchByName(ctx, t.suiteID, t.workerName, request)
	default:
		return nil, ErrWorkerNotFound
	}
}

func (w *Worker) touch() {
	w.mu.Lock()
	w.lastSeen = time.Now()
	w.mu.Unlock()
}

func (w *Worker) registerPending(j *job) {
	w.mu.Lock()
	w.pending[j.request.ID] = j
	w.mu.Unlock()
}

func (w *Worker) unregisterPending(id string) {
	w.mu.Lock()
	delete(w.pending, id)
	w.mu.Unlock()
}

func (w *Worker) isStale(reference time.Time) bool {
	w.mu.Lock()
	last := w.lastSeen
	w.mu.Unlock()
	return reference.Sub(last) > workerStaleAfter
}
