package llm_wiki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	model "github.com/ongridio/ongrid/internal/manager/model/knowledge/llm_wiki"
	"github.com/ongridio/ongrid/internal/pkg/errs"
)

// stateWriteTimeout bounds the detached context used to persist a job's final
// state after the request or worker context is already gone.
const stateWriteTimeout = 10 * time.Second

// jobOutcome is what one compilation produced, as far as the job record cares.
type jobOutcome struct {
	noPagesProduced bool
}

// CreateCompileJob queues one compilation and starts it in the background.
// When sourceIDs is non-empty only those sources are compiled; otherwise the
// full corpus is compiled. A tenant can have at most one active Wiki build job.
func (u *Usecase) CreateCompileJob(ctx context.Context, tenantID uint64, force bool, sourceIDs []uint64) (*model.CompileJob, error) {
	sources, _, err := u.repo.ListSources(ctx, tenantID, "", 1)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, errors.Join(errs.ErrInvalid, errors.New("no wiki sources to compile"))
	}

	var activeKey *string
	if len(sourceIDs) > 0 {
		activeKey = sourceIDActiveKey(tenantID, sourceIDs[0])
	} else {
		activeKey = fullCorpusActiveKey(tenantID)
	}

	job := &model.CompileJob{
		TenantID:     tenantID,
		ActiveKey:    activeKey,
		Status:       model.JobPending,
		Stage:        "queued",
		ForceCompile: force || len(sourceIDs) > 0,
		SourceIDs:    formatSourceIDs(sourceIDs),
	}
	if err := u.repo.CreateJob(ctx, job); err != nil {
		return nil, err
	}
	u.triggerCompile(job)
	return job, nil
}

// RunOnce claims and runs the next pending job.
func (u *Usecase) RunOnce(ctx context.Context, tenantID uint64, owner string, lease time.Duration) error {
	job, err := u.repo.ClaimJob(ctx, tenantID, owner, lease)
	if err != nil {
		return err
	}
	return u.runJob(ctx, job)
}

// ListJobs lists the compile jobs of a tenant, newest first.
func (u *Usecase) ListJobs(ctx context.Context, tenantID uint64, limit int) ([]*model.CompileJob, int64, error) {
	return u.repo.ListJobs(ctx, tenantID, limit)
}

// RetryJob re-queues a finished job and starts it again.
func (u *Usecase) RetryJob(ctx context.Context, tenantID, id uint64) (*model.CompileJob, error) {
	job, err := u.repo.RetryJob(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	u.triggerCompile(job)
	return job, nil
}

// CancelJob requests cancellation of a queued or running job. The worker picks
// the request up at its next cancellation check.
func (u *Usecase) CancelJob(ctx context.Context, tenantID, id uint64) (*model.CompileJob, error) {
	return u.repo.CancelJob(ctx, tenantID, id)
}

// RunJob claims and runs one specific job.
func (u *Usecase) RunJob(ctx context.Context, tenantID, jobID uint64, owner string, lease time.Duration) error {
	job, err := u.repo.ClaimJobByID(ctx, tenantID, jobID, owner, lease)
	if err != nil {
		return err
	}
	return u.runJob(ctx, job)
}

// triggerCompile runs a freshly created job in one bounded goroutine. The
// lifecycle context is used on purpose instead of the HTTP request context: the
// API returns 202 before compilation finishes, so the request context may
// already be canceled when this goroutine starts.
func (u *Usecase) triggerCompile(job *model.CompileJob) {
	if job == nil || u.trigger.Owner == "" {
		return
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				u.log.ErrorContext(u.runCtx, "wiki compile worker panicked",
					slog.Any("panic", recovered),
					slog.Uint64("job_id", job.ID))
			}
		}()
		ctx, cancel := context.WithTimeout(u.runCtx, u.trigger.Timeout)
		defer cancel()
		if err := u.RunJob(ctx, job.TenantID, job.ID, u.trigger.Owner, u.trigger.Timeout+time.Minute); err != nil &&
			!errors.Is(err, context.Canceled) && !errors.Is(err, ErrNoPendingJob) {
			u.log.ErrorContext(ctx, "wiki compile job failed", slog.Uint64("job_id", job.ID), slog.Any("err", err))
		}
	}()
}

// runJob runs one claimed job and always closes its usage session, even when the
// compilation fails.
func (u *Usecase) runJob(ctx context.Context, job *model.CompileJob) error {
	if job == nil {
		return errors.New("llmwiki: compile job is required")
	}
	if u.summarizer == nil {
		return u.failJob(ctx, job, "summarize", errors.New("llmwiki: summarizer is not configured"))
	}
	usageSink := u.startUsageSession(ctx, job.ID)
	defer func() {
		persistCtx, cancel := newStateWriteContext(ctx)
		defer cancel()
		u.closeUsageSession(persistCtx, usageSink)
	}()

	outcome, err := u.runCompilation(ctx, job, usageSink)
	if err != nil {
		return err
	}
	return u.finishJob(ctx, job, outcome)
}

// startUsageSession opens the token accounting session of a job, falling back to
// a no-op sink when no recorder is configured or session creation fails.
func (u *Usecase) startUsageSession(ctx context.Context, jobID uint64) TokenUsageSink {
	if u.usage == nil {
		return noopTokenUsageSink{}
	}
	sink, err := u.usage.Start(ctx, jobID)
	if err != nil {
		u.log.ErrorContext(ctx, "llmwiki: start token usage session failed",
			slog.Uint64("job_id", jobID),
			slog.Any("err", err))
		return noopTokenUsageSink{}
	}
	if sink == nil {
		u.log.ErrorContext(ctx, "llmwiki: token usage recorder returned a nil sink",
			slog.Uint64("job_id", jobID))
		return noopTokenUsageSink{}
	}
	return sink
}

// closeUsageSession closes the usage session without letting accounting
// failures change the compile job's outcome.
func (u *Usecase) closeUsageSession(ctx context.Context, sink TokenUsageSink) {
	if sink == nil {
		return
	}
	if err := sink.Close(ctx); err != nil {
		u.log.ErrorContext(ctx, "llmwiki: close token usage session failed", slog.Any("err", err))
	}
}

// runCompilation compiles the tenant's full corpus into a build and publishes it.
func (u *Usecase) runCompilation(ctx context.Context, job *model.CompileJob, usageSink TokenUsageSink) (jobOutcome, error) {
	outcome := jobOutcome{noPagesProduced: true}

	if u.compiler == nil {
		u.log.WarnContext(ctx, "llmwiki: BuildCompiler not available, skipping compilation")
		return outcome, nil
	}

	compiler := u.compiler.withUsageSink(usageSink)
	compiler.bindJob(job.ID)
	sourceIDs := parseSourceIDs(job.SourceIDs)
	buildResult := compiler.Compile(ctx, job.TenantID, sourceIDs...)
	if !buildResult.Success {
		return outcome, buildResult.Error
	}
	if err := compiler.Publish(ctx, job.TenantID, buildResult.BuildID); err != nil {
		return outcome, err
	}

	outcome.noPagesProduced = buildResult.PageCount == 0
	return outcome, nil
}

// parseSourceIDs parses a JSON array of source IDs from the job's SourceIDs field.
func parseSourceIDs(raw *string) []uint64 {
	if raw == nil || *raw == "" {
		return nil
	}
	var ids []uint64
	if err := json.Unmarshal([]byte(*raw), &ids); err != nil {
		return nil
	}
	return ids
}

// formatSourceIDs serialises source IDs to a JSON array string.
func formatSourceIDs(ids []uint64) *string {
	if len(ids) == 0 {
		return nil
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}

// sourceIDActiveKey returns a stable active_key for a single-source compile job.
func sourceIDActiveKey(tenantID, sourceID uint64) *string {
	key := contentSHA256([]byte(fmt.Sprintf("tenant:%d:source:%d", tenantID, sourceID)))
	s := key
	return &s
}

// fullCorpusActiveKey returns a stable active_key for a full-corpus compile job.
func fullCorpusActiveKey(tenantID uint64) *string {
	key := contentSHA256([]byte(fmt.Sprintf("tenant:%d:full-corpus", tenantID)))
	s := key
	return &s
}

// finishJob records the terminal status of a successful job.
func (u *Usecase) finishJob(ctx context.Context, job *model.CompileJob, outcome jobOutcome) error {
	status, stage := model.JobSucceeded, "completed"
	if outcome.noPagesProduced {
		status, stage = model.JobSkipped, "skipped"
	}
	stateCtx, cancel := newStateWriteContext(ctx)
	defer cancel()
	return u.repo.UpdateJob(stateCtx, job.TenantID, job.ID, status, stage, "")
}

// failJob records the terminal status of a failed job and returns the cause.
func (u *Usecase) failJob(ctx context.Context, job *model.CompileJob, stage string, cause error) error {
	stateCtx, cancel := newStateWriteContext(ctx)
	defer cancel()
	if err := u.repo.UpdateJob(stateCtx, job.TenantID, job.ID, model.JobFailed, stage, truncateErrorMessage(cause)); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// newStateWriteContext detaches from the caller's cancellation and bounds how
// long writing a job's final state may take.
func newStateWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), stateWriteTimeout)
}

// truncateErrorMessage fits an error into the job table's error column.
func truncateErrorMessage(err error) string {
	value := err.Error()
	if len(value) > 2048 {
		return value[:2048]
	}
	return value
}
