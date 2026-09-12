// LLM Wiki HTTP routes and response types.
package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	biz "github.com/ongridio/ongrid/internal/manager/biz/knowledge/llm_wiki"
	model "github.com/ongridio/ongrid/internal/manager/model/knowledge/llm_wiki"
	"github.com/ongridio/ongrid/internal/pkg/errs"
)

type llmWikiService interface {
	ListTree(ctx context.Context, layer, parentID string) ([]biz.TreeNode, error)
	GetNode(ctx context.Context, id string) (*biz.NodeDetail, error)
	PreviewNode(ctx context.Context, id string) (*biz.NodePreview, error)
	DeleteNode(ctx context.Context, id string) error
	ListSources(ctx context.Context, tenantID uint64, status string, limit int) ([]*model.Source, int64, error)
	ListJobs(ctx context.Context, tenantID uint64, limit int) ([]*model.CompileJob, int64, error)
	CreateCompileJob(ctx context.Context, tenantID uint64, sourceIDs []uint64, force bool) (*model.CompileJob, error)
	RetryJob(ctx context.Context, tenantID, id uint64) (*model.CompileJob, error)
	CancelJob(ctx context.Context, tenantID, id uint64) (*model.CompileJob, error)
	Schema() string
	Search(ctx context.Context, tenantID uint64, query string, limit int) ([]biz.SearchHit, error)
	UploadSource(ctx context.Context, filename string, content []byte) (*model.Source, bool, error)
}

func (h *Handler) SetLLMWikiService(svc llmWikiService) { h.llmWikiSvc = svc }

func (h *Handler) wikiWriteMW(next http.Handler) http.Handler {
	if h.authz == nil {
		return next
	}
	return h.authz.Require("knowledge:doc", "write")(next)
}

func (h *Handler) registerLLMWiki(r chi.Router) {
	r.Get("/v1/knowledge/llm-wiki/tree", h.listTree)
	r.Get("/v1/knowledge/llm-wiki/nodes/{id}", h.getNode)
	r.Get("/v1/knowledge/llm-wiki/nodes/{id}/preview", h.preview)
	r.Delete("/v1/knowledge/llm-wiki/nodes/{id}", h.deleteNode)
	r.Get("/v1/knowledge/llm-wiki/schema", h.getSchema)
	r.Get("/v1/knowledge/llm-wiki/sources", h.listSources)
	r.Get("/v1/knowledge/llm-wiki/search", h.searchLLMWiki)
	r.Get("/v1/knowledge/llm-wiki/jobs", h.listJobs)
	r.With(h.wikiWriteMW).Post("/v1/knowledge/llm-wiki/upload", h.upload)
	r.With(h.wikiWriteMW).Post("/v1/knowledge/llm-wiki/compile", h.compile)
	r.With(h.wikiWriteMW).Post("/v1/knowledge/llm-wiki/jobs/{id}/retry", h.retryJob)
	r.With(h.wikiWriteMW).Post("/v1/knowledge/llm-wiki/jobs/{id}/cancel", h.cancelJob)
}

// upload godoc
// @Summary 上传 LLM Wiki 原始文件
// @Success 201 {object} response
// @Router /v1/knowledge/llm-wiki/upload [post]
func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	const maxUploadBytes = biz.MaxSourceBytes
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		writeError(w, errors.Join(errs.ErrInvalid, fmt.Errorf("parse upload: %w", err)))
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, errors.Join(errs.ErrInvalid, fmt.Errorf("multipart field \"file\" required: %w", err)))
		return
	}
	defer file.Close()
	filename := filepath.Base(strings.TrimSpace(header.Filename))
	body, err := io.ReadAll(io.LimitReader(file, maxUploadBytes+1))
	if err != nil {
		writeError(w, err)
		return
	}
	if int64(len(body)) > maxUploadBytes {
		writeError(w, errors.Join(errs.ErrInvalid, fmt.Errorf("file exceeds %d MiB", maxUploadBytes>>20)))
		return
	}
	source, _, err := h.llmWikiSvc.UploadSource(r.Context(), filename, body)
	if err != nil {
		writeError(w, err)
		return
	}
	writeData(w, http.StatusCreated, toSourceDTO(source))
}

// listTree godoc
// @Summary 列出 LLM Wiki 文件树
// @Success 200 {object} response
// @Router /v1/knowledge/llm-wiki/tree [get]
func (h *Handler) listTree(w http.ResponseWriter, r *http.Request) {
	items, err := h.llmWikiSvc.ListTree(r.Context(), r.URL.Query().Get("layer"), r.URL.Query().Get("parent_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	documentCount := 0
	for _, item := range items {
		documentCount += item.DocumentCount
	}
	writeData(w, http.StatusOK, map[string]any{"items": items, "total": len(items), "document_count": documentCount})
}

// getNode godoc
// @Summary 读取 LLM Wiki 节点
// @Success 200 {object} response
// @Router /v1/knowledge/llm-wiki/nodes/{id} [get]
func (h *Handler) getNode(w http.ResponseWriter, r *http.Request) {
	item, err := h.llmWikiSvc.GetNode(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeData(w, http.StatusOK, item)
}

// preview godoc
// @Summary 预览 LLM Wiki Raw PDF/DOCX 文件
// @Success 200 {file} binary
// @Router /v1/knowledge/llm-wiki/nodes/{id}/preview [get]
func (h *Handler) preview(w http.ResponseWriter, r *http.Request) {
	item, err := h.llmWikiSvc.PreviewNode(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", item.ContentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": item.Name}))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(item.Content); err != nil {
		return
	}
}

// deleteNode godoc
// @Summary 删除 LLM Wiki Raw/Wiki 文件
// @Success 200 {object} response
// @Router /v1/knowledge/llm-wiki/nodes/{id} [delete]
func (h *Handler) deleteNode(w http.ResponseWriter, r *http.Request) {
	if err := h.llmWikiSvc.DeleteNode(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeError(w, err)
		return
	}
	writeData(w, http.StatusOK, map[string]bool{"deleted": true})
}

// getSchema godoc
// @Summary 读取 LLM Wiki Schema
// @Success 200 {object} response
// @Router /v1/knowledge/llm-wiki/schema [get]
func (h *Handler) getSchema(w http.ResponseWriter, _ *http.Request) {
	writeData(w, http.StatusOK, map[string]string{"version": biz.SchemaVersion, "content": h.llmWikiSvc.Schema()})
}

// listSources godoc
// @Summary 列出 LLM Wiki 来源
// @Success 200 {object} response
// @Router /v1/knowledge/llm-wiki/sources [get]
func (h *Handler) listSources(w http.ResponseWriter, r *http.Request) {
	rows, total, err := h.llmWikiSvc.ListSources(r.Context(), biz.DefaultTenantID, r.URL.Query().Get("status"), parseLimit(r, 200))
	if err != nil {
		writeError(w, err)
		return
	}
	items := make([]sourceDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, toSourceDTO(row))
	}
	writeData(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

// search godoc
// @Summary 搜索已发布的 LLM Wiki 页面
// @Success 200 {object} response
// @Router /v1/knowledge/llm-wiki/search [get]
func (h *Handler) searchLLMWiki(w http.ResponseWriter, r *http.Request) {
	items, err := h.llmWikiSvc.Search(r.Context(), biz.DefaultTenantID, r.URL.Query().Get("q"), parseLimit(r, 10))
	if err != nil {
		writeError(w, err)
		return
	}
	writeData(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

// listJobs godoc
// @Summary 列出 LLM Wiki 编译任务
// @Success 200 {object} response
// @Router /v1/knowledge/llm-wiki/jobs [get]
func (h *Handler) listJobs(w http.ResponseWriter, r *http.Request) {
	rows, total, err := h.llmWikiSvc.ListJobs(r.Context(), biz.DefaultTenantID, parseLimit(r, 50))
	if err != nil {
		writeError(w, err)
		return
	}
	items := make([]jobDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, toJobDTO(row))
	}
	writeData(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

// compile godoc
// @Summary 创建 LLM Wiki 编译任务
// @Success 202 {object} response
// @Router /v1/knowledge/llm-wiki/compile [post]
func (h *Handler) compile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceIDs []string `json:"source_ids"`
		Force     bool     `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	ids, err := parseIDs(req.SourceIDs)
	if err != nil {
		writeError(w, err)
		return
	}
	job, err := h.llmWikiSvc.CreateCompileJob(r.Context(), biz.DefaultTenantID, ids, req.Force)
	if err != nil {
		writeError(w, err)
		return
	}
	writeData(w, http.StatusAccepted, toJobDTO(job))
}

// retryJob godoc
// @Summary 重试 LLM Wiki 编译任务
// @Success 200 {object} response
// @Router /v1/knowledge/llm-wiki/jobs/{id}/retry [post]
func (h *Handler) retryJob(w http.ResponseWriter, r *http.Request) {
	id, err := biz.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, err)
		return
	}
	job, err := h.llmWikiSvc.RetryJob(r.Context(), biz.DefaultTenantID, id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeData(w, http.StatusOK, toJobDTO(job))
}

// cancelJob godoc
// @Summary 取消 LLM Wiki 编译任务
// @Success 200 {object} response
// @Router /v1/knowledge/llm-wiki/jobs/{id}/cancel [post]
func (h *Handler) cancelJob(w http.ResponseWriter, r *http.Request) {
	id, err := biz.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, err)
		return
	}
	job, err := h.llmWikiSvc.CancelJob(r.Context(), biz.DefaultTenantID, id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeData(w, http.StatusOK, toJobDTO(job))
}

type response struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type sourceDTO struct {
	ID               string    `json:"id"`
	SourceKey        string    `json:"source_key"`
	SourceType       string    `json:"source_type"`
	RawPath          string    `json:"raw_path"`
	CurrentVersionID string    `json:"current_version_id,omitempty"`
	Status           string    `json:"status"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type jobDTO struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Stage     string    `json:"stage"`
	SourceIDs []string  `json:"source_ids"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toSourceDTO(row *model.Source) sourceDTO {
	out := sourceDTO{ID: strconv.FormatUint(row.ID, 10), SourceKey: row.SourceKey, SourceType: row.SourceType, RawPath: row.RawPath, Status: row.Status, UpdatedAt: row.UpdatedAt}
	if row.CurrentVersionID != nil {
		out.CurrentVersionID = strconv.FormatUint(*row.CurrentVersionID, 10)
	}
	return out
}

func toJobDTO(row *model.CompileJob) jobDTO {
	ids, err := biz.DecodeSourceIDs(row.SourceIDsJSON)
	if err != nil {
		// The persisted job remains visible even if legacy/corrupt source IDs
		// cannot be decoded; the worker reports the authoritative failure.
		ids = nil
	}
	outIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		outIDs = append(outIDs, strconv.FormatUint(id, 10))
	}
	return jobDTO{ID: strconv.FormatUint(row.ID, 10), Status: row.Status, Stage: row.Stage, SourceIDs: outIDs, Error: row.ErrorMessage, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func parseIDs(raw []string) ([]uint64, error) {
	ids := make([]uint64, 0, len(raw))
	for _, value := range raw {
		id, err := biz.ParseID(value)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func parseLimit(r *http.Request, fallback int) int {
	value, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func writeData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(response{Code: "ok", Message: "ok", Data: data}); err != nil {
		return
	}
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := "internal"
	message := "internal error"
	if errors.Is(err, errs.ErrInvalid) {
		status = http.StatusBadRequest
		code = "invalid_argument"
		message = err.Error()
	} else if errors.Is(err, errs.ErrNotFound) {
		status = http.StatusNotFound
		code = "not_found"
		message = "not found"
	} else if errors.Is(err, errs.ErrConflict) {
		status = http.StatusConflict
		code = "conflict"
		message = err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if encodeErr := json.NewEncoder(w).Encode(response{Code: code, Message: message}); encodeErr != nil {
		return
	}
}
