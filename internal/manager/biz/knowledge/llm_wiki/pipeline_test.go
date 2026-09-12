package llm_wiki

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	model "github.com/ongridio/ongrid/internal/manager/model/knowledge/llm_wiki"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSource_WhenMarkdownIsValid_ReturnsParseIR(t *testing.T) {
	input := &sourceIR{
		source:  &model.Source{RawPath: "notes.md"},
		version: &model.SourceVersion{ID: 7},
		body:    []byte("# Title\n\nGrounded content."),
	}

	parsed, err := parseSource(input)

	require.NoError(t, err)
	require.Len(t, parsed.chunks, 1)
	assert.Equal(t, input.source, parsed.source)
	assert.Equal(t, input.version, parsed.version)
	assert.Equal(t, string(input.body), parsed.markdown)
	assert.Equal(t, string(input.body), parsed.chunks[0].Text)
}

func TestParseSource_WhenIRIsIncomplete_ReturnsError(t *testing.T) {
	tests := []struct {
		name  string
		input *sourceIR
	}{
		{name: "nil IR"},
		{name: "missing source", input: &sourceIR{version: &model.SourceVersion{}}},
		{name: "missing version", input: &sourceIR{source: &model.Source{}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := parseSource(test.input)

			assert.Nil(t, parsed)
			require.EqualError(t, err, "llmwiki: source IR is incomplete")
		})
	}
}

type pipelineIndexer struct {
	documents []IndexDocument
	err       error
}

func (i *pipelineIndexer) IndexPage(_ context.Context, document IndexDocument) error {
	i.documents = append(i.documents, document)
	return i.err
}

func (*pipelineIndexer) DeletePage(context.Context, *model.Page) error { return nil }

func (*pipelineIndexer) Search(context.Context, uint64, string, int) ([]SearchHit, error) {
	return nil, nil
}

func TestIndexArtifacts_SkipsUnchangedDocumentsAndReportsFailure(t *testing.T) {
	indexer := &pipelineIndexer{err: errors.New("index unavailable")}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	usecase := &Usecase{indexer: indexer, log: logger}
	documents := []IndexDocument{
		{Page: &model.Page{PageID: "cached"}, SkipIndex: true},
		{Page: &model.Page{PageID: "fresh"}},
	}

	failed := usecase.indexArtifacts(context.Background(), documents)

	assert.True(t, failed)
	require.Len(t, indexer.documents, 1)
	assert.Equal(t, "fresh", indexer.documents[0].Page.PageID)
}
