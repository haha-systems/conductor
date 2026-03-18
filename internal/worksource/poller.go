package worksource

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/haha-systems/conductor/internal/domain"
)

// PollerConfig controls TaskPoller behaviour.
type PollerConfig struct {
	IntervalSeconds   int
	MaxConcurrentRuns int
}

// TaskPoller polls a WorkSource on a fixed interval and emits claimed tasks.
type TaskPoller struct {
	source  WorkSource
	cfg     PollerConfig
	mu      sync.Mutex
	running int
}

// NewPoller creates a TaskPoller.
func NewPoller(source WorkSource, cfg PollerConfig) *TaskPoller {
	if cfg.IntervalSeconds <= 0 {
		cfg.IntervalSeconds = 30
	}
	if cfg.MaxConcurrentRuns <= 0 {
		cfg.MaxConcurrentRuns = 4
	}
	return &TaskPoller{source: source, cfg: cfg}
}

// Run starts the polling loop and sends claimed tasks to the returned channel.
// The channel is closed when ctx is cancelled.
func (p *TaskPoller) Run(ctx context.Context) <-chan *domain.Task {
	out := make(chan *domain.Task, p.cfg.MaxConcurrentRuns)

	go func() {
		defer close(out)
		ticker := time.NewTicker(time.Duration(p.cfg.IntervalSeconds) * time.Second)
		defer ticker.Stop()

		// Poll once immediately on start.
		p.poll(ctx, out)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.poll(ctx, out)
			}
		}
	}()

	return out
}

// Done must be called after a task's run completes to release the concurrency slot.
func (p *TaskPoller) Done() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running > 0 {
		p.running--
	}
}

func (p *TaskPoller) poll(ctx context.Context, out chan<- *domain.Task) {
	issueTasks, err := p.source.Poll(ctx)
	if err != nil {
		slog.Error("poll failed", "source", p.source.Name(), "error", err)
	}

	prTasks, err := p.source.ListOpenPRs(ctx)
	if err != nil {
		slog.Error("list open PRs failed", "source", p.source.Name(), "error", err)
	}

	tasks := append(issueTasks, prTasks...)

	for _, task := range tasks {
		if !p.tryAcquireSlot() {
			// Back-pressure: at capacity, leave task for next poll.
			slog.Debug("at capacity, skipping task", "task_id", task.ID)
			return
		}

		if err := p.source.Claim(ctx, task); err != nil {
			p.releaseSlot()
			slog.Error("claim failed", "task_id", task.ID, "error", err)
			continue
		}

		task.Status = domain.TaskStatusClaimed
		slog.Info("task claimed", "task_id", task.ID, "source", p.source.Name())

		select {
		case out <- task:
		case <-ctx.Done():
			p.releaseSlot()
			return
		}
	}
}

func (p *TaskPoller) tryAcquireSlot() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running >= p.cfg.MaxConcurrentRuns {
		return false
	}
	p.running++
	return true
}

func (p *TaskPoller) releaseSlot() {
	p.Done()
}

// CurrentRunning returns the number of in-flight runs.
func (p *TaskPoller) CurrentRunning() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

