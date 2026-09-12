package knowledge

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	biz "github.com/ongridio/ongrid/internal/manager/biz/knowledge/llm_wiki"
	model "github.com/ongridio/ongrid/internal/manager/model/knowledge/llm_wiki"
)

type serviceStub struct {
	compiled   []uint64
	uploaded   string
	uploadBody []byte
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

func (s *serviceStub) CreateCompileJob(_ context.Context, _ uint64, sourceIDs []uint64, _ bool) (*model.CompileJob, error) {
	s.compiled = sourceIDs
	return &model.CompileJob{ID: 9007199254740993, SourceIDsJSON: `[9007199254740993]`, Status: model.JobPending, Stage: "queued"}, nil
}

func (*serviceStub) RetryJob(context.Context, uint64, uint64) (*model.CompileJob, error) {
	return nil, nil
}

func (*serviceStub) CancelJob(context.Context, uint64, uint64) (*model.CompileJob, error) {
	return nil, nil
}

func (*serviceStub) Schema() string { return "schema" }

func (*serviceStub) Search(context.Context, uint64, string, int) ([]biz.SearchHit, error) {
	return nil, nil
}

func (s *serviceStub) UploadSource(_ context.Context, filename string, content []byte) (*model.Source, bool, error) {
	s.uploaded = filename
	s.uploadBody = content
	return &model.Source{ID: 7, SourceKey: "upload:" + filename, SourceType: "upload", RawPath: "upload/guide.md", Status: model.SourcePending}, true, nil
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
	req := httptest.NewRequest(http.MethodPost, "/v1/knowledge/llm-wiki/compile", strings.NewReader(`{"source_ids":["9007199254740993"]}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if len(svc.compiled) != 1 || svc.compiled[0] != 9007199254740993 {
		t.Fatalf("compiled IDs = %v", svc.compiled)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"code":"ok"`) || !strings.Contains(body, `"id":"9007199254740993"`) {
		t.Fatalf("response = %s", body)
	}
}

func TestUpload_UsesDedicatedWikiSource(t *testing.T) {
	svc := &serviceStub{}
	var body bytes.Buffer
	multipartWriter := multipart.NewWriter(&body)
	part, err := multipartWriter.CreateFormFile("file", "guide.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("# Guide")); err != nil {
		t.Fatal(err)
	}
	if err := multipartWriter.Close(); err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	newLLMWikiHandler(svc).Register(router)
	req := httptest.NewRequest(http.MethodPost, "/v1/knowledge/llm-wiki/upload", &body)
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if svc.uploaded != "guide.md" || string(svc.uploadBody) != "# Guide" {
		t.Fatalf("uploaded = %q, body = %q", svc.uploaded, svc.uploadBody)
	}
	if !strings.Contains(response.Body.String(), `"source_type":"upload"`) {
		t.Fatalf("response = %s", response.Body.String())
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
