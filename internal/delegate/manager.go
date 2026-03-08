package delegate

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

const maxTaskLogBytes = 32 * 1024

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
)

type ProgressFunc func(line string)

type Runner interface {
	RunTask(ctx context.Context, opts StartOptions, progress ProgressFunc) (string, error)
}

type StartOptions struct {
	CodespaceName string
	Workdir       string
	ExecAgent     string
	Prompt        string
	Model         string
	Cwd           string
	OnComplete    func(context.Context)
}

type TaskSnapshot struct {
	ID            string
	CodespaceName string
	Cwd           string
	Prompt        string
	Model         string
	Status        Status
	Result        string
	Error         string
	Log           string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type TaskManager interface {
	StartTask(opts StartOptions) (string, error)
	GetTask(id string) (TaskSnapshot, error)
	CancelTask(id string) error
}

type taskState struct {
	snapshot TaskSnapshot
	cancel   context.CancelFunc
}

type Manager struct {
	mu     sync.RWMutex
	nextID int
	now    func() time.Time
	runner Runner
	tasks  map[string]*taskState
}

func NewManager(runner Runner) *Manager {
	return &Manager{
		now:    time.Now,
		runner: runner,
		tasks:  make(map[string]*taskState),
	}
}

func (m *Manager) StartTask(opts StartOptions) (string, error) {
	if strings.TrimSpace(opts.Prompt) == "" {
		return "", fmt.Errorf("prompt must not be empty")
	}
	if opts.CodespaceName == "" {
		return "", fmt.Errorf("codespace name is required")
	}
	if opts.Cwd == "" {
		opts.Cwd = opts.Workdir
	}

	now := m.now()

	m.mu.Lock()
	defer m.mu.Unlock()

	m.nextID++
	id := fmt.Sprintf("delegate-%d", m.nextID)

	ctx, cancel := context.WithCancel(context.Background())
	state := &taskState{
		snapshot: TaskSnapshot{
			ID:            id,
			CodespaceName: opts.CodespaceName,
			Cwd:           opts.Cwd,
			Prompt:        opts.Prompt,
			Model:         opts.Model,
			Status:        StatusQueued,
			CreatedAt:     now,
			UpdatedAt:     now,
		},
		cancel: cancel,
	}
	m.tasks[id] = state

	go m.runTask(ctx, id, opts)

	return id, nil
}

func (m *Manager) GetTask(id string) (TaskSnapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	state, ok := m.tasks[id]
	if !ok {
		return TaskSnapshot{}, fmt.Errorf("delegate task %q not found", id)
	}
	return state.snapshot, nil
}

func (m *Manager) CancelTask(id string) error {
	m.mu.RLock()
	state, ok := m.tasks[id]
	var status Status
	var cancel context.CancelFunc
	if ok {
		status = state.snapshot.Status
		cancel = state.cancel
	}
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("delegate task %q not found", id)
	}

	switch status {
	case StatusCompleted, StatusFailed, StatusCanceled:
		return fmt.Errorf("delegate task %q is already %s", id, status)
	}

	cancel()
	return nil
}

func (m *Manager) runTask(ctx context.Context, id string, opts StartOptions) {
	m.updateTask(id, func(snapshot *TaskSnapshot) {
		snapshot.Status = StatusRunning
		snapshot.UpdatedAt = m.now()
	})

	result, err := m.runner.RunTask(ctx, opts, func(line string) {
		if strings.TrimSpace(line) == "" {
			return
		}
		m.updateTask(id, func(snapshot *TaskSnapshot) {
			snapshot.Log = appendTaskLog(snapshot.Log, line)
			snapshot.UpdatedAt = m.now()
		})
	})

	switch {
	case ctx.Err() == context.Canceled:
		m.updateTask(id, func(snapshot *TaskSnapshot) {
			snapshot.Status = StatusCanceled
			snapshot.UpdatedAt = m.now()
			snapshot.Log = appendTaskLog(snapshot.Log, "Task canceled.")
		})
	case err != nil:
		m.updateTask(id, func(snapshot *TaskSnapshot) {
			snapshot.Status = StatusFailed
			snapshot.Error = err.Error()
			snapshot.UpdatedAt = m.now()
		})
	default:
		m.updateTask(id, func(snapshot *TaskSnapshot) {
			snapshot.Status = StatusCompleted
			snapshot.Result = result
			snapshot.UpdatedAt = m.now()
		})
	}

	if opts.OnComplete != nil && ctx.Err() == nil {
		callbackCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		opts.OnComplete(callbackCtx)
	}
}

func (m *Manager) updateTask(id string, update func(snapshot *TaskSnapshot)) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, ok := m.tasks[id]
	if !ok {
		return
	}
	update(&state.snapshot)
}

func appendTaskLog(existing, line string) string {
	updated := strings.TrimRight(existing, "\n")
	if updated != "" {
		updated += "\n"
	}
	updated += line
	if len(updated) <= maxTaskLogBytes {
		return updated
	}
	return updated[len(updated)-maxTaskLogBytes:]
}
