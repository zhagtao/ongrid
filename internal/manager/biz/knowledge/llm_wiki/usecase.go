package llm_wiki

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	model "github.com/ongridio/ongrid/internal/manager/model/knowledge/llm_wiki"
	"github.com/ongridio/ongrid/internal/pkg/docextract"
	"github.com/ongridio/ongrid/internal/pkg/errs"
)

type Usecase struct {
	repo       Repository
	files      *FileStore
	summarizer Summarizer
	indexer    Indexer
	log        *slog.Logger
	runCtx     context.Context
	trigger    CompileTriggerOption
	usage      TokenUsageRecorder
	limits     PlannerConfig
}

func New(ctx context.Context, repo Repository, files *FileStore, summarizer Summarizer, indexer Indexer, log *slog.Logger, trigger ...CompileTriggerOption) (*Usecase, error) {
	return newUsecase(ctx, repo, files, summarizer, indexer, log, nil, trigger...)
}

// NewWithUsageRecorder wires Wiki compilation to the existing AIOps chat
// transcript accounting without adding a Wiki-specific statistics table.
func NewWithUsageRecorder(ctx context.Context, repo Repository, files *FileStore, summarizer Summarizer, indexer Indexer, log *slog.Logger, usage TokenUsageRecorder, trigger ...CompileTriggerOption) (*Usecase, error) {
	return newUsecase(ctx, repo, files, summarizer, indexer, log, usage, trigger...)
}

func newUsecase(ctx context.Context, repo Repository, files *FileStore, summarizer Summarizer, indexer Indexer, log *slog.Logger, usage TokenUsageRecorder, trigger ...CompileTriggerOption) (*Usecase, error) {
	if repo == nil || files == nil {
		return nil, errors.New("llmwiki: repository and file store are required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if log == nil {
		log = slog.Default()
	}
	if err := files.Ensure(ctx); err != nil {
		return nil, err
	}
	var compileTrigger CompileTriggerOption
	if len(trigger) > 0 {
		compileTrigger = trigger[0]
	}
	if compileTrigger.Timeout <= 0 {
		compileTrigger.Timeout = 10 * time.Minute
	}
	usecase := &Usecase{
		repo:       repo,
		files:      files,
		summarizer: summarizer,
		indexer:    indexer,
		log:        log,
		runCtx:     ctx,
		trigger:    compileTrigger,
		usage:      usage,
		limits:     DefaultPlannerConfig(),
	}
	if compileTrigger.Limits != nil {
		usecase.limits = *compileTrigger.Limits
	}
	if err := usecase.Reconcile(ctx, DefaultTenantID); err != nil {
		return nil, err
	}
	return usecase, nil
}

func (u *Usecase) Reconcile(ctx context.Context, tenantID uint64) error {
	if err := u.reconcileManifests(ctx, tenantID); err != nil {
		return err
	}
	sources, _, err := u.repo.ListSources(ctx, tenantID, "", 500)
	if err != nil {
		return fmt.Errorf("llmwiki: list sources for reconcile: %w", err)
	}
	for _, source := range sources {
		if source.CurrentVersionID == nil {
			continue
		}
		version, err := u.repo.GetVersion(ctx, tenantID, *source.CurrentVersionID)
		if err != nil {
			return err
		}
		body, err := u.files.Read(ctx, version.SnapshotPath, MaxSourceBytes)
		if err != nil {
			if markErr := u.repo.MarkSourceStatus(ctx, tenantID, source.ID, model.SourceFailed); markErr != nil {
				return errors.Join(err, markErr)
			}
			continue
		}
		rawPath, err := u.files.resolve("raw", source.RawPath)
		if err != nil {
			return err
		}
		if _, err := os.Stat(rawPath); errors.Is(err, fs.ErrNotExist) {
			if err := atomicWrite(rawPath, body, 0o640); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	return u.refreshDerivedArtifacts(ctx, tenantID, WikiLogEvent{})
}

func (u *Usecase) refreshDerivedArtifacts(ctx context.Context, tenantID uint64, event WikiLogEvent) error {
	pages, _, warnings, err := u.files.RefreshCatalogFiles(ctx, event)
	if err != nil {
		return fmt.Errorf("llmwiki: refresh wiki catalog files: %w", err)
	}
	if rebuilder, ok := u.indexer.(CatalogRebuilder); ok {
		rebuildWarnings, rebuildErr := rebuilder.Rebuild(ctx, tenantID, u.files)
		if rebuildErr != nil {
			return fmt.Errorf("llmwiki: rebuild derived wiki indexes: %w", rebuildErr)
		}
		warnings = append(warnings, rebuildWarnings...)
		if event.ID == "" {
			digest := sha256.New()
			for _, page := range pages {
				digest.Write([]byte(page.ID))
				digest.Write([]byte{0})
				digest.Write([]byte(page.BodySHA256))
				digest.Write([]byte{0})
			}
			rebuildEvent := WikiLogEvent{ID: "rebuild-index-" + hex.EncodeToString(digest.Sum(nil))[:16], OccurredAt: time.Now().UTC(), Action: "rebuild-index", Subject: "wiki index", UpdatedPages: len(pages)}
			if _, _, _, err := u.files.RefreshCatalogFiles(ctx, rebuildEvent); err != nil {
				return fmt.Errorf("llmwiki: write index rebuild log: %w", err)
			}
		}
	}
	for _, warning := range warnings {
		u.log.WarnContext(ctx, "wiki derived artifact warning", slog.String("detail", warning))
	}
	return nil
}

func (u *Usecase) reconcileManifests(ctx context.Context, tenantID uint64) error {
	unlock := u.files.LockArtifacts()
	defer unlock()
	manifests, err := u.files.ListManifests(ctx)
	if err != nil {
		return err
	}
	for _, manifest := range manifests {
		if manifest.TenantID != tenantID {
			continue
		}
		complete := true
		bodies := make(map[string][]byte, len(manifest.Pages))
		for _, page := range manifest.Pages {
			body, readErr := u.files.ReadStagedPage(ctx, manifest.JobID, page.RelativePath)
			if errors.Is(readErr, fs.ErrNotExist) {
				storagePath, pathErr := pageStorageRelative(page)
				if pathErr != nil {
					complete = false
					break
				}
				body, readErr = u.files.Read(ctx, storagePath, MaxPageBytes)
			}
			if readErr != nil || contentSHA256(body) != page.BodySHA256 {
				complete = false
				break
			}
			bodies[page.PageID] = body
		}
		if !complete {
			continue
		}
		if manifest.PageSourceVersionIDs == nil {
			manifest.PageSourceVersionIDs = make(map[string][]uint64, len(manifest.Pages))
		}
		sourcePageID := ""
		for _, page := range manifest.Pages {
			versionIDs := manifest.PageSourceVersionIDs[page.PageID]
			if len(versionIDs) == 0 {
				versionIDs = []uint64{manifest.VersionID}
			}
			manifest.PageSourceVersionIDs[page.PageID] = versionIDs
			if page.PageType == model.PageTypeSource {
				sourcePageID = page.PageID
			}
		}
		deletedIDs := make([]string, 0, len(manifest.DeletedPages))
		for _, page := range manifest.DeletedPages {
			if page != nil {
				deletedIDs = append(deletedIDs, page.PageID)
			}
		}
		batch := makeArtifactBatch(manifest.Pages, manifest.PageSourceVersionIDs, manifest.Relations, deletedIDs)
		if err := u.repo.CommitTopicCompilation(ctx, tenantID, manifest.SourceID, manifest.VersionID, sourcePageID, manifest.Topics, manifest.Evidence, batch); err != nil {
			return fmt.Errorf("llmwiki: reconcile compilation: %w", err)
		}
		if err := u.files.ActivateManifest(ctx, manifest); err != nil {
			return fmt.Errorf("llmwiki: reconcile artifact activation: %w", err)
		}
		for _, page := range manifest.DeletedPages {
			if page == nil {
				continue
			}
			if err := u.files.RemovePage(ctx, page.PageType, page.RelativePath); err != nil {
				return err
			}
			if u.indexer != nil {
				if err := u.indexer.DeletePage(ctx, page); err != nil {
					u.log.WarnContext(ctx, "wiki reconciled stale page index delete failed", slog.String("page_id", page.PageID), slog.Any("err", err))
				}
			}
		}
		_, _, warnings, err := u.files.RefreshCatalogFiles(ctx, manifest.LogEvent)
		if err != nil {
			return fmt.Errorf("llmwiki: reconcile catalog files: %w", err)
		}
		for _, warning := range warnings {
			u.log.WarnContext(ctx, "wiki broken link ignored", slog.String("link", warning))
		}
		if err := u.repo.UpdateJob(ctx, tenantID, manifest.JobID, model.JobSucceeded, "reconciled", ""); err != nil && !errors.Is(err, errs.ErrNotFound) {
			return err
		}
		indexFailed := false
		if u.indexer != nil {
			for _, page := range manifest.Pages {
				versionIDs := manifest.PageSourceVersionIDs[page.PageID]
				if len(versionIDs) == 0 {
					versionIDs = []uint64{manifest.VersionID}
				}
				document := IndexDocument{Page: page, Content: string(bodies[page.PageID]), SourceVersionIDs: versionIDs}
				if page.PageType == model.PageTypeTopic {
					topic, topicErr := u.repo.GetTopic(ctx, tenantID, page.PageID)
					if topicErr != nil {
						return fmt.Errorf("llmwiki: load reconciled topic %q: %w", page.PageID, topicErr)
					}
					document.TopicID = topic.TopicID
					document.Aliases = decodeStringList(topic.AliasesJSON)
					document.EntityNames = decodeStringList(topic.EntityNamesJSON)
					document.ConceptNames = decodeStringList(topic.ConceptNamesJSON)
				}
				if err := u.indexer.IndexPage(ctx, document); err != nil {
					indexFailed = true
					u.log.WarnContext(ctx, "wiki reconciled page index failed", slog.String("page_id", page.PageID), slog.Any("err", err))
				}
			}
		}
		if indexFailed {
			continue
		}
		if err := u.files.FinishManifest(ctx, manifest.JobID); err != nil {
			return err
		}
	}
	return nil
}

func (u *Usecase) Mirror(ctx context.Context, in MirrorInput) (*model.Source, bool, error) {
	mirrored, err := u.files.Mirror(ctx, in)
	if err != nil {
		return nil, false, err
	}
	source := &model.Source{
		TenantID:      in.TenantID,
		SourceKey:     in.SourceKey,
		SourceType:    in.SourceType,
		RawPath:       mirrored.RawPath,
		ContentSHA256: mirrored.SHA256,
		Status:        model.SourcePending,
	}
	version := &model.SourceVersion{
		TenantID:      in.TenantID,
		SHA256:        mirrored.SHA256,
		SizeBytes:     mirrored.Size,
		SnapshotPath:  mirrored.SnapshotPath,
		SchemaVersion: SchemaVersion,
	}
	got, _, changed, err := u.repo.UpsertSourceVersion(ctx, source, version)
	if err != nil {
		return nil, false, fmt.Errorf("llmwiki: save mirror metadata: %w", err)
	}
	return got, changed, nil
}

// UploadSource stores one LLM Wiki source through the dedicated upload path.
// It deliberately does not pass through the organization knowledge usecase.
func (u *Usecase) UploadSource(ctx context.Context, filename string, content []byte) (*model.Source, bool, error) {
	filename = strings.TrimSpace(filename)
	if filename == "." || filename == "" {
		return nil, false, errors.Join(errs.ErrInvalid, errors.New("filename is required"))
	}
	filename = safeUploadName(filename)
	if !docextract.Supported(filename) {
		return nil, false, errors.Join(errs.ErrInvalid, errors.New("only text, markdown, PDF, and DOCX files are supported"))
	}
	if len(content) == 0 {
		return nil, false, errors.Join(errs.ErrInvalid, errors.New("file is empty"))
	}
	if _, err := docextract.Extract(filename, content); err != nil {
		return nil, false, errors.Join(errs.ErrInvalid, err)
	}
	return u.Mirror(ctx, MirrorInput{
		TenantID:   DefaultTenantID,
		SourceKey:  "upload:" + filename,
		SourceType: "upload",
		Name:       filename,
		Content:    content,
	})
}

func (u *Usecase) DeleteNode(ctx context.Context, id string) error {
	unlock := u.files.LockArtifacts()
	defer unlock()
	layer, relative, err := decodeNodeID(id)
	if err != nil {
		return errors.Join(errs.ErrInvalid, errors.New("invalid wiki file"))
	}
	if layer == "wiki" {
		return u.deleteWikiPage(ctx, relative)
	}
	if layer != "raw" {
		return errors.Join(errs.ErrInvalid, errors.New("only raw or wiki files can be deleted"))
	}
	sources, _, err := u.repo.ListSources(ctx, DefaultTenantID, "", 500)
	if err != nil {
		return fmt.Errorf("llmwiki: list sources for delete: %w", err)
	}
	var source *model.Source
	for _, candidate := range sources {
		if candidate.RawPath == relative {
			source = candidate
			break
		}
	}
	if source == nil {
		return errs.ErrNotFound
	}
	pages, err := u.repo.ListPagesBySource(ctx, DefaultTenantID, source.ID)
	if err != nil {
		return fmt.Errorf("llmwiki: list pages for source delete: %w", err)
	}
	if err := u.repo.DeleteSource(ctx, DefaultTenantID, source.ID); err != nil {
		return err
	}

	var cleanupErr error
	for _, page := range pages {
		if err := u.files.RemovePage(ctx, page.PageType, page.RelativePath); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
		if u.indexer != nil {
			if err := u.indexer.DeletePage(ctx, page); err != nil {
				cleanupErr = errors.Join(cleanupErr, err)
			}
		}
	}
	if err := u.files.RemoveRaw(ctx, source.RawPath); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if cleanupErr != nil {
		return fmt.Errorf("llmwiki: source %q deleted but file/index cleanup failed: %w", relative, cleanupErr)
	}
	return u.refreshDerivedArtifacts(ctx, DefaultTenantID, WikiLogEvent{ID: fmt.Sprintf("delete-source-%d-%d", source.ID, time.Now().UnixNano()), OccurredAt: time.Now().UTC(), Action: "delete-source", Subject: filepath.Base(relative), DeletedPages: len(pages)})
}

func (u *Usecase) deleteWikiPage(ctx context.Context, relative string) error {
	pages, err := u.repo.ListPages(ctx, DefaultTenantID)
	if err != nil {
		return fmt.Errorf("llmwiki: list wiki pages for delete: %w", err)
	}
	var page *model.Page
	for _, candidate := range pages {
		if candidate.RelativePath == relative && candidate.PageType == model.PageTypeTopic {
			page = candidate
			break
		}
	}
	if page == nil {
		return errs.ErrNotFound
	}
	if err := u.repo.DeletePage(ctx, DefaultTenantID, page.PageID); err != nil {
		return err
	}
	var cleanupErr error
	if err := u.files.RemovePage(ctx, page.PageType, page.RelativePath); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if u.indexer != nil {
		if err := u.indexer.DeletePage(ctx, page); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if cleanupErr != nil {
		return fmt.Errorf("llmwiki: wiki page %q deleted but file/index cleanup failed: %w", relative, cleanupErr)
	}
	return u.refreshDerivedArtifacts(ctx, DefaultTenantID, WikiLogEvent{ID: fmt.Sprintf("delete-topic-%s-%d", page.PageID, time.Now().UnixNano()), OccurredAt: time.Now().UTC(), Action: "delete-topic", Subject: page.Title, DeletedPages: 1})
}

func (u *Usecase) CreateCompileJob(ctx context.Context, tenantID uint64, sourceIDs []uint64, force bool) (*model.CompileJob, error) {
	if len(sourceIDs) == 0 {
		sources, _, err := u.repo.ListSources(ctx, tenantID, "", 500)
		if err != nil {
			return nil, err
		}
		for _, source := range sources {
			sourceIDs = append(sourceIDs, source.ID)
		}
	}
	if len(sourceIDs) == 0 {
		return nil, errors.Join(errs.ErrInvalid, errors.New("no wiki sources to compile"))
	}
	sort.Slice(sourceIDs, func(i, j int) bool { return sourceIDs[i] < sourceIDs[j] })
	sourceIDs = dedupeIDs(sourceIDs)
	for _, id := range sourceIDs {
		if _, err := u.repo.GetSource(ctx, tenantID, id); err != nil {
			return nil, fmt.Errorf("llmwiki: source %d: %w", id, err)
		}
	}
	body, err := json.Marshal(sourceIDs)
	if err != nil {
		return nil, fmt.Errorf("llmwiki: encode source ids: %w", err)
	}
	activeDigest := sha256.Sum256(append([]byte(fmt.Sprintf("%d:", tenantID)), body...))
	activeKey := hex.EncodeToString(activeDigest[:])
	job := &model.CompileJob{
		TenantID:      tenantID,
		SourceIDsJSON: string(body),
		ActiveKey:     &activeKey,
		Status:        model.JobPending,
		Stage:         "queued",
		ForceCompile:  force,
	}
	if err := u.repo.CreateJob(ctx, job); err != nil {
		return nil, err
	}
	u.triggerCompile(job)
	return job, nil
}

func (u *Usecase) RunOnce(ctx context.Context, tenantID uint64, owner string, lease time.Duration) error {
	job, err := u.repo.ClaimJob(ctx, tenantID, owner, lease)
	if err != nil {
		return err
	}
	return u.runJob(ctx, job)
}

func (u *Usecase) RunJob(ctx context.Context, tenantID, jobID uint64, owner string, lease time.Duration) error {
	job, err := u.repo.ClaimJobByID(ctx, tenantID, jobID, owner, lease)
	if err != nil {
		return err
	}
	return u.runJob(ctx, job)
}

// triggerCompile starts one bounded goroutine for the newly-created job. The
// lifecycle context is deliberately used instead of the HTTP request context:
// the API returns 202 before compilation finishes, so the request context may
// already be cancelled when this goroutine starts.
func (u *Usecase) triggerCompile(job *model.CompileJob) {
	if job == nil || strings.TrimSpace(u.trigger.Owner) == "" {
		return
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				u.log.ErrorContext(u.runCtx, "wiki compile worker panicked", slog.Any("panic", recovered), slog.Uint64("job_id", job.ID))
			}
		}()
		ctx, cancel := context.WithTimeout(u.runCtx, u.trigger.Timeout)
		defer cancel()
		if err := u.RunJob(ctx, job.TenantID, job.ID, u.trigger.Owner, u.trigger.Timeout+time.Minute); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrNoPendingJob) {
			u.log.ErrorContext(ctx, "wiki compile job failed", slog.Uint64("job_id", job.ID), slog.Any("err", err))
		}
	}()
}

func (u *Usecase) runJob(ctx context.Context, job *model.CompileJob) (runErr error) {
	if job == nil {
		return errors.New("llmwiki: compile job is required")
	}
	if u.summarizer == nil {
		return u.failJob(ctx, job, "summarize", errors.New("llmwiki: summarizer is not configured"))
	}
	sourceIDs, err := DecodeSourceIDs(job.SourceIDsJSON)
	if err != nil {
		return u.failJob(ctx, job, "decode", err)
	}
	usageSink, err := u.startUsageSession(ctx, job.ID)
	if err != nil {
		return u.failJob(ctx, job, "usage", err)
	}
	defer func() {
		runErr = u.closeUsageSession(context.WithoutCancel(ctx), job, usageSink, runErr)
	}()

	result, err := u.compileJobSources(ctx, job, sourceIDs, usageSink)
	if err != nil {
		return err
	}
	return u.finishJob(ctx, job, result)
}

type jobRunResult struct {
	allSourcesSkipped bool
	indexFailed       bool
}

func (u *Usecase) startUsageSession(ctx context.Context, jobID uint64) (TokenUsageSink, error) {
	if u.usage == nil {
		return noopTokenUsageSink{}, nil
	}
	sink, err := u.usage.Start(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if sink == nil {
		return nil, errors.New("llmwiki: token usage recorder returned a nil sink")
	}
	return sink, nil
}

func (u *Usecase) closeUsageSession(ctx context.Context, job *model.CompileJob, sink TokenUsageSink, runErr error) error {
	closeErr := sink.Close(ctx)
	if closeErr == nil {
		return runErr
	}
	closeErr = fmt.Errorf("close token usage session: %w", closeErr)
	if runErr != nil {
		return errors.Join(runErr, closeErr)
	}
	return u.failJob(ctx, job, "usage_close", closeErr)
}

func (u *Usecase) compileJobSources(ctx context.Context, job *model.CompileJob, sourceIDs []uint64, usageSink TokenUsageSink) (jobRunResult, error) {
	result := jobRunResult{allSourcesSkipped: true}
	for _, sourceID := range sourceIDs {
		if err := u.checkJobCancellation(ctx, job); err != nil {
			return result, err
		}

		documents, err := u.compileSource(ctx, job, sourceID, usageSink)
		if err != nil {
			return result, u.handleSourceCompileError(ctx, job, sourceID, err)
		}
		if len(documents) > 0 {
			result.allSourcesSkipped = false
		}
		result.indexFailed = u.indexArtifacts(ctx, documents) || result.indexFailed
	}
	return result, nil
}

func (u *Usecase) checkJobCancellation(ctx context.Context, job *model.CompileJob) error {
	cancelled, err := u.repo.IsCancelRequested(ctx, job.TenantID, job.ID)
	if err != nil {
		return u.failJob(ctx, job, "cancel_check", err)
	}
	if !cancelled {
		return nil
	}
	if err := u.repo.UpdateJob(ctx, job.TenantID, job.ID, model.JobCancelled, "cancelled", ""); err != nil {
		return err
	}
	return context.Canceled
}

func (u *Usecase) handleSourceCompileError(ctx context.Context, job *model.CompileJob, sourceID uint64, compileErr error) error {
	if errors.Is(compileErr, context.Canceled) {
		if err := u.repo.MarkSourceStatus(ctx, job.TenantID, sourceID, model.SourcePending); err != nil {
			compileErr = errors.Join(compileErr, err)
		}
		if err := u.repo.UpdateJob(ctx, job.TenantID, job.ID, model.JobCancelled, "cancelled", ""); err != nil {
			return errors.Join(compileErr, err)
		}
		return compileErr
	}
	if err := u.repo.MarkSourceStatus(ctx, job.TenantID, sourceID, model.SourceFailed); err != nil {
		compileErr = errors.Join(compileErr, err)
	}
	return u.failJob(ctx, job, "compile", compileErr)
}

func (u *Usecase) finishJob(ctx context.Context, job *model.CompileJob, result jobRunResult) error {
	status, stage := model.JobSucceeded, "completed"
	if result.allSourcesSkipped {
		status, stage = model.JobSkipped, "skipped"
	}
	if result.indexFailed {
		stage = "index_failed"
	}
	return u.repo.UpdateJob(ctx, job.TenantID, job.ID, status, stage, "")
}

func (u *Usecase) failJob(ctx context.Context, job *model.CompileJob, stage string, cause error) error {
	if err := u.repo.UpdateJob(ctx, job.TenantID, job.ID, model.JobFailed, stage, truncateError(cause)); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (u *Usecase) compileSource(ctx context.Context, job *model.CompileJob, sourceID uint64, usageSink TokenUsageSink) ([]IndexDocument, error) {
	return newCompilePipeline(u).compile(ctx, job, sourceID, usageSink)
}

func contentSHA256(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func validateGeneration(documents []IndexDocument, relations []*model.PageRelation) error {
	allowedPageTypes := map[string]bool{model.PageTypeSource: true, model.PageTypeTopic: true}
	allowedRelationTypes := map[string]bool{"wikilink": true, "mentions": true, "depends_on": true, "related_to": true, "conflicts_with": true, "supersedes": true}
	pageIDs := make(map[string]bool, len(documents))
	for _, document := range documents {
		if document.Page == nil || !allowedPageTypes[document.Page.PageType] || strings.TrimSpace(document.Page.PageID) == "" || len(document.Page.PageID) > 64 || strings.TrimSpace(document.Page.Title) == "" || len([]rune(document.Page.Title)) > 512 || strings.TrimSpace(document.Content) == "" || len(document.Content) > MaxPageBytes || len(document.SourceVersionIDs) == 0 {
			return errors.Join(errs.ErrInvalid, errors.New("llmwiki: invalid generated page"))
		}
		if pageIDs[document.Page.PageID] {
			return errors.Join(errs.ErrInvalid, errors.New("llmwiki: duplicate generated page id"))
		}
		pageIDs[document.Page.PageID] = true
	}
	for _, relation := range relations {
		if relation == nil || !allowedRelationTypes[relation.RelationType] || !pageIDs[relation.FromPageID] || !pageIDs[relation.ToPageID] {
			return errors.Join(errs.ErrInvalid, errors.New("llmwiki: invalid generated page relation"))
		}
	}
	return nil
}

func buildSourcePage(source *model.Source, version *model.SourceVersion, summaries []ChunkSummaryIR, fileSummary *ChunkSummaryIR) (*model.Page, string) {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", source.TenantID, source.SourceKey)))
	id := "source-" + hex.EncodeToString(sum[:12])
	title := filepath.Base(source.RawPath)
	relative := filepath.ToSlash(filepath.Join("sources", filepath.Dir(source.RawPath), strings.TrimSuffix(title, filepath.Ext(title))+".md"))
	page := &model.Page{
		PageID:       id,
		TenantID:     source.TenantID,
		PageType:     "source",
		Title:        title,
		AliasesJSON:  "[]",
		Language:     "und",
		RelativePath: relative,
		CreatedAt:    version.CreatedAt,
		UpdatedAt:    version.CreatedAt,
	}
	var content strings.Builder
	description := ""
	if fileSummary != nil {
		description = strings.TrimSpace(fileSummary.Summary)
	}
	created := version.CreatedAt.UTC()
	if created.IsZero() {
		created = time.Now().UTC()
	}
	fmt.Fprintf(&content, "---\nid: %s\ntype: source\ntitle: %q\ndescription: %q\naliases: []\nlanguage: und\nsource_versions: [%d]\nrelated: []\nsource_file: %q\ncreated: %s\nupdated: %s\n---\n\n# %s\n", id, title, description, version.ID, source.RawPath, created.Format(time.DateOnly), created.Format(time.DateOnly), title)
	if fileSummary != nil {
		fmt.Fprintf(&content, "\n## 概览\n\n%s\n", fileSummary.Summary)
	}
	for i, summary := range summaries {
		fmt.Fprintf(&content, "\n## 分段 %d\n\n%s\n", i+1, summary.Summary)
		if len(summary.Facts) > 0 {
			content.WriteString("\n### 事实与证据\n")
			for _, fact := range summary.Facts {
				fmt.Fprintf(&content, "\n- %s（证据 rune %d..%d）", fact.Statement, fact.EvidenceStart, fact.EvidenceEnd)
			}
			content.WriteByte('\n')
		}
		if len(summary.Conflicts) > 0 {
			content.WriteString("\n### 保留的冲突\n")
			for _, conflict := range summary.Conflicts {
				fmt.Fprintf(&content, "\n- %s；冲突对象：%s", conflict.Statement, conflict.With)
			}
			content.WriteByte('\n')
		}
	}
	return page, content.String()
}

func pagePath(page *model.Page) string {
	return filepath.ToSlash(strings.TrimSpace(page.RelativePath))
}

func pageStorageRelative(page *model.Page) (string, error) {
	layer, err := pageStorageLayer(page.PageType)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Join(layer, page.RelativePath)), nil
}

func (u *Usecase) ListSources(ctx context.Context, tenantID uint64, status string, limit int) ([]*model.Source, int64, error) {
	return u.repo.ListSources(ctx, tenantID, status, limit)
}

func (u *Usecase) ListJobs(ctx context.Context, tenantID uint64, limit int) ([]*model.CompileJob, int64, error) {
	return u.repo.ListJobs(ctx, tenantID, limit)
}

func (u *Usecase) RetryJob(ctx context.Context, tenantID, id uint64) (*model.CompileJob, error) {
	job, err := u.repo.RetryJob(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	u.triggerCompile(job)
	return job, nil
}

func (u *Usecase) CancelJob(ctx context.Context, tenantID, id uint64) (*model.CompileJob, error) {
	return u.repo.CancelJob(ctx, tenantID, id)
}

func (u *Usecase) Schema() string {
	return BuiltinSchema()
}

func (u *Usecase) ReadPage(ctx context.Context, tenantID uint64, pageID string) (*model.Page, string, error) {
	unlock := u.files.LockArtifacts()
	defer unlock()
	pages, err := u.repo.ListPages(ctx, tenantID)
	if err != nil {
		return nil, "", err
	}
	for _, page := range pages {
		if page.PageID == pageID {
			storagePath, err := pageStorageRelative(page)
			if err != nil {
				return nil, "", err
			}
			body, err := u.files.Read(ctx, storagePath, MaxPageBytes)
			return page, string(body), err
		}
	}
	return nil, "", errs.ErrNotFound
}

func (u *Usecase) Search(ctx context.Context, tenantID uint64, query string, limit int) ([]SearchHit, error) {
	unlock := u.files.LockArtifacts()
	defer unlock()
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	if u.indexer != nil {
		hits, err := u.indexer.Search(ctx, tenantID, query, limit)
		if err == nil {
			return hits, nil
		}
		u.log.WarnContext(ctx, "wiki index search failed; falling back to files", slog.Any("err", err))
	}
	pages, err := u.repo.ListPages(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	topics, err := u.repo.ListTopics(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	topicByID := make(map[string]*model.Topic, len(topics))
	for _, topic := range topics {
		topicByID[topic.TopicID] = topic
	}
	hits := make([]SearchHit, 0)
	terms := strings.Fields(query)
	for _, page := range pages {
		storagePath, err := pageStorageRelative(page)
		if err != nil {
			continue
		}
		body, err := u.files.Read(ctx, storagePath, MaxPageBytes)
		if err != nil {
			continue
		}
		nodeNames := make([]string, 0)
		if topic := topicByID[page.PageID]; topic != nil {
			nodeNames = append(nodeNames, decodeStringList(topic.EntityNamesJSON)...)
			nodeNames = append(nodeNames, decodeStringList(topic.ConceptNamesJSON)...)
		}
		haystack := strings.ToLower(page.Title + "\n" + page.AliasesJSON + "\n" + strings.Join(nodeNames, "\n") + "\n" + string(body))
		matches := 0
		for _, term := range terms {
			if strings.Contains(haystack, term) {
				matches++
			}
		}
		if matches == 0 {
			continue
		}
		score := float64(matches) / float64(len(terms))
		if strings.Contains(strings.ToLower(page.Title), query) {
			score += 0.25
		}
		if page.PageType == model.PageTypeTopic {
			score += 0.1
		}
		preview := string(body)
		if len([]rune(preview)) > 800 {
			preview = string([]rune(preview)[:800]) + "…"
		}
		hits = append(hits, SearchHit{Layer: "wiki", PageType: page.PageType, PageID: page.PageID, Title: page.Title, Preview: preview, Score: score, MatchedNode: findMatchedKnowledgeNode(query, nodeNames)})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func findMatchedKnowledgeNode(query string, names []string) string {
	terms := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	for _, name := range names {
		value := strings.ToLower(strings.TrimSpace(name))
		if value == "" {
			continue
		}
		for _, term := range terms {
			if strings.Contains(value, term) || strings.Contains(term, value) {
				return name
			}
		}
	}
	return ""
}

func (u *Usecase) ListTree(ctx context.Context, layer, parentID string) ([]TreeNode, error) {
	unlock := u.files.LockArtifacts()
	defer unlock()
	relative := ""
	if parentID != "" {
		decodedLayer, decodedRelative, err := decodeNodeID(parentID)
		if err != nil || decodedLayer != layer {
			return nil, errors.Join(errs.ErrInvalid, errors.New("invalid parent"))
		}
		relative = decodedRelative
	}
	if layer == "schema" {
		if parentID != "" {
			return nil, nil
		}
		info, err := os.Stat(filepath.Join(u.files.Root(), "schema.md"))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		updated := info.ModTime()
		return []TreeNode{{ID: encodeNodeID("schema", "schema.md"), Layer: "schema", Kind: "file", Name: "schema.md", RelativePath: "schema.md", DocumentCount: 1, UpdatedAt: &updated}}, nil
	}
	if layer != "raw" && layer != "wiki" {
		return nil, errors.Join(errs.ErrInvalid, errors.New("invalid layer"))
	}
	dir := filepath.Join(u.files.Root(), layer, filepath.FromSlash(relative))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	items := make([]TreeNode, 0, len(entries))
	sourceByPath := make(map[string]*model.Source)
	pageByPath := make(map[string]*model.Page)
	if layer == "raw" {
		sources, _, listErr := u.repo.ListSources(ctx, DefaultTenantID, "", 500)
		if listErr != nil {
			return nil, listErr
		}
		for _, source := range sources {
			sourceByPath[source.RawPath] = source
		}
	}
	if layer == "wiki" {
		pages, listErr := u.repo.ListPages(ctx, DefaultTenantID)
		if listErr != nil {
			return nil, listErr
		}
		for _, page := range pages {
			pageByPath[page.RelativePath] = page
		}
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		rel := filepath.ToSlash(filepath.Join(relative, entry.Name()))
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		node := TreeNode{ID: encodeNodeID(layer, rel), ParentID: parentID, Layer: layer, Kind: "file", Name: entry.Name(), RelativePath: rel}
		if source := sourceByPath[rel]; source != nil {
			node.SourceID = strconv.FormatUint(source.ID, 10)
			node.Status = source.Status
		}
		if page := pageByPath[rel]; page != nil {
			node.PageID = page.PageID
			node.PageType = page.PageType
			node.Name = page.Title
			node.Status = "ready"
		}
		if entry.IsDir() {
			node.Kind = "folder"
			documentCount, countErr := countDocuments(filepath.Join(dir, entry.Name()))
			if countErr != nil {
				return nil, countErr
			}
			node.DocumentCount = documentCount
			children, readErr := os.ReadDir(filepath.Join(dir, entry.Name()))
			if readErr == nil {
				node.ChildCount = len(children)
				node.HasChildren = len(children) > 0
			}
		}
		if node.Kind == "file" {
			node.DocumentCount = 1
		}
		updated := info.ModTime()
		node.UpdatedAt = &updated
		items = append(items, node)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return items[i].Kind == "folder"
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	return items, nil
}

func countDocuments(root string) (int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("llmwiki: count documents in %s: %w", root, err)
	}
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if entry.IsDir() {
			nested, err := countDocuments(path)
			if err != nil {
				return 0, err
			}
			count += nested
			continue
		}
		count++
	}
	return count, nil
}

func (u *Usecase) GetNode(ctx context.Context, id string) (*NodeDetail, error) {
	unlock := u.files.LockArtifacts()
	defer unlock()
	layer, relative, err := decodeNodeID(id)
	if err != nil {
		return nil, err
	}
	if layer != "raw" && layer != "wiki" && layer != "schema" {
		return nil, errs.ErrInvalid
	}
	path, err := u.files.resolve(layer, relative)
	if err != nil {
		return nil, err
	}
	storeRelative := filepath.ToSlash(filepath.Join(layer, relative))
	maxBytes := int64(MaxPageBytes)
	if layer == "raw" {
		maxBytes = MaxSourceBytes
	}
	if layer == "schema" {
		if relative != "schema.md" {
			return nil, errs.ErrInvalid
		}
		path, err = u.files.resolve("", "schema.md")
		if err != nil {
			return nil, err
		}
		storeRelative = "schema.md"
	}
	body, err := u.files.Read(ctx, storeRelative, maxBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errs.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, errors.Join(errs.ErrInvalid, errors.New("folder has no content"))
	}
	sum := sha256.Sum256(body)
	updated := info.ModTime()
	displayBody := body
	if layer == "raw" && rawPreviewType(relative) != "" {
		displayBody = nil
	}
	detail := &NodeDetail{TreeNode: TreeNode{ID: id, Layer: layer, Kind: "file", Name: filepath.Base(relative), RelativePath: relative, UpdatedAt: &updated}, Content: string(displayBody), Metadata: map[string]any{"sha256": hex.EncodeToString(sum[:])}}
	if layer == "raw" {
		sources, _, listErr := u.repo.ListSources(ctx, DefaultTenantID, "", 500)
		if listErr != nil {
			return nil, listErr
		}
		for _, source := range sources {
			if source.RawPath == relative {
				detail.SourceID = strconv.FormatUint(source.ID, 10)
				detail.Status = source.Status
				detail.Metadata["source_type"] = source.SourceType
				if source.CurrentVersionID != nil {
					detail.Metadata["source_version"] = strconv.FormatUint(*source.CurrentVersionID, 10)
				}
				if err := u.addSourceWikiMetadata(ctx, detail.Metadata, source.ID); err != nil {
					return nil, err
				}
				break
			}
		}
	}
	if layer == "wiki" {
		pages, listErr := u.repo.ListPages(ctx, DefaultTenantID)
		if listErr != nil {
			return nil, listErr
		}
		for _, page := range pages {
			if page.RelativePath == relative {
				detail.PageID = page.PageID
				detail.PageType = page.PageType
				detail.Status = "ready"
				detail.Metadata["page_type"] = page.PageType
				var aliases []string
				if err := json.Unmarshal([]byte(page.AliasesJSON), &aliases); err == nil {
					detail.Metadata["aliases"] = aliases
				}
				if page.PageType == model.PageTypeTopic {
					topic, topicErr := u.repo.GetTopic(ctx, page.TenantID, page.PageID)
					if topicErr != nil {
						return nil, topicErr
					}
					detail.Metadata["entities"] = decodeStringList(topic.EntityNamesJSON)
					detail.Metadata["concepts"] = decodeStringList(topic.ConceptNamesJSON)
				}
				if err := u.addPageSourceMetadata(ctx, detail.Metadata, page); err != nil {
					return nil, err
				}
				break
			}
		}
	}
	return detail, nil
}

func (u *Usecase) PreviewNode(ctx context.Context, id string) (*NodePreview, error) {
	unlock := u.files.LockArtifacts()
	defer unlock()
	layer, relative, err := decodeNodeID(id)
	if err != nil || layer != "raw" {
		return nil, errors.Join(errs.ErrInvalid, errors.New("only raw files can be previewed"))
	}
	previewType := rawPreviewType(relative)
	if previewType == "" {
		return nil, errors.Join(errs.ErrInvalid, errors.New("only PDF and DOCX files support binary preview"))
	}
	storeRelative := filepath.ToSlash(filepath.Join("raw", relative))
	body, err := u.files.Read(ctx, storeRelative, MaxSourceBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errs.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	contentType := "application/pdf"
	if previewType == "docx" {
		text, extractErr := docextract.ExtractPlainText(relative, body)
		if extractErr != nil {
			return nil, fmt.Errorf("llmwiki: preview source %q: %w", relative, extractErr)
		}
		body = []byte(text)
		contentType = "text/plain; charset=utf-8"
	}
	return &NodePreview{Name: filepath.Base(relative), ContentType: contentType, Content: body}, nil
}

func rawPreviewType(relative string) string {
	switch strings.ToLower(filepath.Ext(relative)) {
	case ".pdf":
		return "pdf"
	case ".docx":
		return "docx"
	default:
		return ""
	}
}

func (u *Usecase) addPageSourceMetadata(ctx context.Context, metadata map[string]any, page *model.Page) error {
	refs, err := u.repo.ListPageSources(ctx, page.TenantID, page.PageID)
	if err != nil {
		return err
	}
	type sourceReference struct {
		Path      string `json:"path"`
		NodeID    string `json:"node_id"`
		VersionID string `json:"version_id"`
	}
	references := make([]sourceReference, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		version, err := u.repo.GetVersion(ctx, page.TenantID, ref.SourceVersionID)
		if err != nil {
			return err
		}
		source, err := u.repo.GetSource(ctx, page.TenantID, version.SourceID)
		if err != nil {
			return err
		}
		if seen[source.RawPath] {
			continue
		}
		seen[source.RawPath] = true
		references = append(references, sourceReference{
			Path:      source.RawPath,
			NodeID:    encodeNodeID("raw", source.RawPath),
			VersionID: strconv.FormatUint(version.ID, 10),
		})
	}
	if len(references) == 0 {
		return nil
	}
	metadata["source_files"] = references
	metadata["source_file"] = references[0].Path
	metadata["source_node_id"] = references[0].NodeID
	metadata["source_version"] = references[0].VersionID
	return nil
}

func (u *Usecase) addSourceWikiMetadata(ctx context.Context, metadata map[string]any, sourceID uint64) error {
	pages, err := u.repo.ListPagesBySource(ctx, DefaultTenantID, sourceID)
	if err != nil {
		return err
	}
	type wikiReference struct {
		Title  string `json:"title"`
		Path   string `json:"path"`
		NodeID string `json:"node_id"`
		PageID string `json:"page_id"`
	}
	references := make([]wikiReference, 0, len(pages))
	for _, page := range pages {
		references = append(references, wikiReference{
			Title:  page.Title,
			Path:   page.RelativePath,
			NodeID: encodeNodeID("wiki", page.RelativePath),
			PageID: page.PageID,
		})
	}
	metadata["wiki_files"] = references
	return nil
}

func encodeNodeID(layer, relative string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(layer + ":" + relative))
}

func decodeNodeID(id string) (string, string, error) {
	body, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", "", errors.Join(errs.ErrInvalid, err)
	}
	parts := strings.SplitN(string(body), ":", 2)
	if len(parts) != 2 {
		return "", "", errs.ErrInvalid
	}
	relative := filepath.Clean(filepath.FromSlash(parts[1]))
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", errs.ErrInvalid
	}
	return parts[0], filepath.ToSlash(relative), nil
}

func dedupeIDs(ids []uint64) []uint64 {
	out := ids[:0]
	for _, id := range ids {
		if len(out) == 0 || out[len(out)-1] != id {
			out = append(out, id)
		}
	}
	return out
}

func truncateError(err error) string {
	value := err.Error()
	if len(value) > 2048 {
		return value[:2048]
	}
	return value
}

func ParseID(id string) (uint64, error) {
	value, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return 0, errors.Join(errs.ErrInvalid, err)
	}
	return value, nil
}
