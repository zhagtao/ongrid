package llm_wiki

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ongridio/ongrid/internal/pkg/errs"
)

func TestValidateWikiArticle_AcceptsMultipleEvidenceChunks(t *testing.T) {
	evidence := TopicEvidence{Evidence: []EvidenceItem{
		{SourceID: 1, SourceVersionID: 10, ChunkOrdinal: 0, Text: "Definition"},
		{SourceID: 1, SourceVersionID: 10, ChunkOrdinal: 1, Text: "Implementation"},
		{SourceID: 2, SourceVersionID: 20, ChunkOrdinal: 0, Text: "Limitation"},
	}}
	raw := `{"title":"Foo","summary":"Grounded summary","sections":[{"title":"Overview","body":"Definition","evidence_refs":[{"source_version_id":10,"chunk_ordinal":0}]},{"title":"Implementation","body":"Implementation","evidence_refs":[{"source_version_id":10,"chunk_ordinal":1}]},{"title":"Limitations","body":"Limitation","evidence_refs":[{"source_version_id":20,"chunk_ordinal":0}]}],"aliases":[],"limitations":[]}`
	article, err := validateWikiArticle(raw, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if len(article.Sections) != 3 {
		t.Fatalf("section count = %d", len(article.Sections))
	}
}

func TestValidateWikiArticle_RejectsEvidenceOutsideInput(t *testing.T) {
	evidence := TopicEvidence{Evidence: []EvidenceItem{{SourceVersionID: 10, ChunkOrdinal: 0, Text: "Foo supports A and B."}}}
	raw := `{"title":"Foo","summary":"Foo","sections":[{"title":"Overview","body":"Foo also supports C.","evidence_refs":[{"source_version_id":10,"chunk_ordinal":9}]}],"aliases":[],"limitations":[]}`
	if _, err := validateWikiArticle(raw, evidence); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("unknown evidence error = %v", err)
	}
}

func TestValidateWikiArticle_RejectsEvidenceStartOutsideInput(t *testing.T) {
	evidence := TopicEvidence{Evidence: []EvidenceItem{{SourceVersionID: 10, ChunkOrdinal: 0, Text: "grounded"}}}
	raw := `{"title":"Foo","summary":"Foo","sections":[{"title":"Overview","body":"Foo","evidence_refs":[{"source_version_id":10,"chunk_ordinal":0,"evidence_start":99,"evidence_end":0}]}],"aliases":[],"limitations":[]}`
	if _, err := validateWikiArticle(raw, evidence); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("out-of-bounds evidence start error = %v", err)
	}
}

func TestMergeEffectiveEvidence_ReplacesSameSourceAndRetainsIndependentConflict(t *testing.T) {
	now := time.Now().UTC()
	existing := TopicEvidence{Evidence: []EvidenceItem{
		{SourceID: 1, SourceVersionID: 11, ChunkOrdinal: 0, Text: "dimension=512", SourceUpdatedAt: now.Add(-time.Hour)},
		{SourceID: 2, SourceVersionID: 21, ChunkOrdinal: 0, Text: "dimension=1024", SourceUpdatedAt: now},
	}}
	incoming := TopicEvidence{Topic: TopicPlan{Importance: 1}, Evidence: []EvidenceItem{{SourceID: 1, SourceVersionID: 12, ChunkOrdinal: 0, Text: "dimension=768", SourceUpdatedAt: now.Add(time.Hour)}}}
	merged := mergeEffectiveEvidence(existing, incoming, 1, 12000)
	if len(merged.Evidence) != 2 {
		t.Fatalf("evidence = %+v, want current source plus independent source", merged.Evidence)
	}
	for _, item := range merged.Evidence {
		if item.SourceVersionID == 11 {
			t.Fatalf("superseded evidence retained: %+v", merged.Evidence)
		}
	}
}

func TestWriterFingerprintIsDeterministicAndEvidenceSensitive(t *testing.T) {
	evidence := TopicEvidence{Evidence: []EvidenceItem{{SourceVersionID: 7, ChunkOrdinal: 1, Text: "A"}, {SourceVersionID: 7, ChunkOrdinal: 0, Text: "B"}}}
	first := generationFingerprint(evidence, "model-v1")
	evidence.Evidence[0], evidence.Evidence[1] = evidence.Evidence[1], evidence.Evidence[0]
	if second := generationFingerprint(evidence, "model-v1"); second != first {
		t.Fatalf("fingerprint changed with evidence order: %q != %q", second, first)
	}
	evidence.Evidence[0].Text = "changed"
	if changed := generationFingerprint(evidence, "model-v1"); changed == first {
		t.Fatal("fingerprint did not change with evidence")
	}
}

func TestWriterFingerprintIgnoresProvenanceOnlyChanges(t *testing.T) {
	first := TopicEvidence{Topic: TopicPlan{ID: "topic-1", Title: "Topic"}, Evidence: []EvidenceItem{{SourceID: 9, SourceVersionID: 10, ChunkOrdinal: 2, Text: "same"}}}
	second := first
	second.Evidence = []EvidenceItem{{SourceID: 9, SourceVersionID: 11, ChunkOrdinal: 0, Text: "same"}}
	if generationFingerprint(first, "model-v1") != generationFingerprint(second, "model-v1") {
		t.Fatal("fingerprint changed when only source version and chunk ordinal changed")
	}
}

func TestUpdateTopicPageSourceVersions(t *testing.T) {
	body := "---\nid: topic-1\nsource_versions: [1]\n---\n\n# Topic\n"
	updated := updateTopicPageSourceVersions(body, []uint64{2, 3})
	if !strings.Contains(updated, "source_versions: [2, 3]\n") || strings.Contains(updated, "source_versions: [1]\n") {
		t.Fatalf("updated frontmatter = %q", updated)
	}
	without := "---\nid: topic-1\n---\n\n# Topic\n"
	updated = updateTopicPageSourceVersions(without, []uint64{4})
	if !strings.Contains(updated, "source_versions: [4]\n") {
		t.Fatalf("inserted frontmatter = %q", updated)
	}
}

func TestWriterPromptRequiresEvidenceOnlyAndUnsupportedSectionOmission(t *testing.T) {
	for _, phrase := range []string{"Use only information supported", "Omit unsupported sections", "Never cite evidence outside"} {
		if !strings.Contains(writerSystemPrompt, phrase) {
			t.Fatalf("writer prompt missing %q", phrase)
		}
	}
}
