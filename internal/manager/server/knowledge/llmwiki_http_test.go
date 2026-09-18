package knowledge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	biz "github.com/ongridio/ongrid/internal/manager/biz/knowledge/llm_wiki"
	model "github.com/ongridio/ongrid/internal/manager/model/knowledge/llm_wiki"
)

type serviceStub struct {
	compiled bool
	tenantID uint64
	force    bool
}

func (*serviceStub) ListTree(context.Context, string, string) ([]biz.TreeNode, error) {
	return nil, nil
}

func (*serviceStub) GetNode(context.Context, string) (*biz.NodeDetail, error) { return nil, nil }

func (*serviceStub) PreviewNode(context.Context, string) (*biz.NodePreview, error) {
	return &biz.NodePreview{Name: "guide.pdf", ContentType: "application/pdf", Content: []byte("%PDF")}, nil
}

func (*serviceStub) DeleteNode(context.Context, string) error { return nil }

func (*serviceStub) ListSources(context.Context, uint64, string, int) ([]*model.Source, int64, error) {
	return nil, 0, nil
}

func (*serviceStub) ListJobs(context.Context, uint64, int) ([]*model.CompileJob, int64, error) {
	return nil, 0, nil
}

func (s *serviceStub) CreateCompileJob(_ context.Context, tenantID uint64, force bool, sourceIDs []uint64) (*model.CompileJob, error) {
	s.compiled = true
	s.tenantID = tenantID
	s.force = force
	return &model.CompileJob{ID: 9007199254740993, ForceCompile: force, Status: model.JobPending, Stage: "queued"}, nil
}

func (*serviceStub) RetryJob(context.Context, uint64, uint64) (*model.CompileJob, error) {
	return nil, nil
}

func (*serviceStub) CancelJob(context.Context, uint64, uint64) (*model.CompileJob, error) {
	return nil, nil
}

func (*serviceStub) Search(context.Context, uint64, string, int) ([]biz.SearchHit, error) {
	return nil, nil
}

func (*serviceStub) SyncOrganizationSources(context.Context, []biz.OrganizationSource) (*biz.SyncResult, error) {
	return &biz.SyncResult{}, nil
}

func newLLMWikiHandler(svc *serviceStub) *Handler {
	h := NewHandler(nil)
	h.SetLLMWikiService(svc)
	return h
}

func TestCompile_ReturnsEnvelopeAndStringIDs(t *testing.T) {
	svc := &serviceStub{}
	router := chi.NewRouter()
	newLLMWikiHandler(svc).Register(router)
	req := httptest.NewRequest(http.MethodPost, "/v1/knowledge/llm-wiki/compile", strings.NewReader(`{"force":true}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if !svc.compiled || svc.tenantID != biz.DefaultTenantID || !svc.force {
		t.Fatalf("compile request = %+v", svc)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"code":"ok"`) || !strings.Contains(body, `"id":"9007199254740993"`) {
		t.Fatalf("response = %s", body)
	}
}

func TestPreview_ReturnsInlineBinaryContent(t *testing.T) {
	router := chi.NewRouter()
	newLLMWikiHandler(&serviceStub{}).Register(router)
	req := httptest.NewRequest(http.MethodGet, "/v1/knowledge/llm-wiki/nodes/raw-guide/preview", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/pdf" {
		t.Fatalf("content type = %q", got)
	}
	if got := response.Header().Get("Content-Disposition"); !strings.Contains(got, `inline`) || !strings.Contains(got, `guide.pdf`) {
		t.Fatalf("content disposition = %q", got)
	}
	if got := response.Body.String(); got != "%PDF" {
		t.Fatalf("body = %q", got)
	}
}

func TestDeleteNode_ReturnsSuccessEnvelope(t *testing.T) {
	router := chi.NewRouter()
	newLLMWikiHandler(&serviceStub{}).Register(router)
	req := httptest.NewRequest(http.MethodDelete, "/v1/knowledge/llm-wiki/nodes/raw-guide", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"deleted":true`) {
		t.Fatalf("response = %s", response.Body.String())
	}
}
