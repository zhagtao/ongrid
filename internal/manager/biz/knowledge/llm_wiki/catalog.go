package llm_wiki

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	initialWikiIndex = "# Wiki Index\n\n## Sources\n\n## Topics\n"
	initialWikiLog   = "# Wiki Log\n"
)

type wikiFrontmatter struct {
	ID             string   `yaml:"id"`
	Type           string   `yaml:"type"`
	Title          string   `yaml:"title"`
	Description    string   `yaml:"description"`
	Aliases        []string `yaml:"aliases"`
	Language       string   `yaml:"language"`
	SourceVersions []uint64 `yaml:"source_versions"`
	Related        []string `yaml:"related"`
}

var wikilinkRE = regexp.MustCompile(`\[\[([^\]|]+)(?:\|[^\]]*)?\]\]`)

// Catalog reads Wiki Markdown. index.md and log.md are presentation files,
// never catalog entries or search documents.
func (s *FileStore) Catalog(ctx context.Context) ([]WikiPage, []WikiLink, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	wikiRoot, err := s.resolve("", "wiki")
	if err != nil {
		return nil, nil, nil, err
	}
	pages := make([]WikiPage, 0)
	bodies := make(map[string]string)
	err = filepath.WalkDir(wikiRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("llmwiki: symlink in wiki catalog rejected: %s", path)
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}
		relative, err := filepath.Rel(wikiRoot, path)
		if err != nil {
			return fmt.Errorf("llmwiki: resolve catalog path: %w", err)
		}
		relative = filepath.ToSlash(relative)
		if relative == "index.md" || relative == "log.md" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("llmwiki: read catalog page %s: %w", relative, err)
		}
		page, err := parseWikiPage(relative, body)
		if err != nil {
			return err
		}
		if _, duplicate := bodies[page.ID]; duplicate {
			return fmt.Errorf("llmwiki: duplicate page id %q", page.ID)
		}
		pages = append(pages, page)
		bodies[page.ID] = string(body)
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}
	links, warnings := resolveWikiLinks(pages, bodies)
	return pages, links, warnings, nil
}

func parseWikiPage(relative string, body []byte) (WikiPage, error) {
	if !strings.HasPrefix(string(body), "---\n") {
		return WikiPage{}, fmt.Errorf("llmwiki: page %s has no frontmatter", relative)
	}
	end := strings.Index(string(body[4:]), "\n---")
	if end < 0 {
		return WikiPage{}, fmt.Errorf("llmwiki: page %s has unterminated frontmatter", relative)
	}
	var frontmatter wikiFrontmatter
	if err := yaml.Unmarshal(body[4:4+end], &frontmatter); err != nil {
		return WikiPage{}, fmt.Errorf("llmwiki: decode frontmatter %s: %w", relative, err)
	}
	frontmatter.ID = strings.TrimSpace(frontmatter.ID)
	frontmatter.Type = strings.TrimSpace(frontmatter.Type)
	frontmatter.Title = strings.TrimSpace(frontmatter.Title)
	if frontmatter.ID == "" || frontmatter.Title == "" {
		return WikiPage{}, fmt.Errorf("llmwiki: page %s requires id and title", relative)
	}
	if frontmatter.Type != "source" && frontmatter.Type != "topic" {
		return WikiPage{}, fmt.Errorf("llmwiki: page %s has invalid type %q", relative, frontmatter.Type)
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return WikiPage{}, fmt.Errorf("llmwiki: page path traversal rejected: %s", relative)
	}
	sum := sha256.Sum256(body)
	return WikiPage{
		ID: frontmatter.ID, Type: frontmatter.Type, Title: frontmatter.Title,
		Description: strings.TrimSpace(frontmatter.Description), Aliases: cleanStrings(frontmatter.Aliases),
		Language: strings.TrimSpace(frontmatter.Language), SourceVersions: frontmatter.SourceVersions,
		Related: cleanStrings(frontmatter.Related), RelativePath: filepath.ToSlash(clean), BodySHA256: hex.EncodeToString(sum[:]),
	}, nil
}

func resolveWikiLinks(pages []WikiPage, bodies map[string]string) ([]WikiLink, []string) {
	byPath := make(map[string]string, len(pages))
	byName := make(map[string]string, len(pages)*2)
	for _, page := range pages {
		path := strings.TrimSuffix(filepath.ToSlash(page.RelativePath), ".md")
		byPath[normalizeWikiTarget(path)] = page.ID
		byName[normalizeWikiTarget(page.Title)] = page.ID
		for _, alias := range page.Aliases {
			byName[normalizeWikiTarget(alias)] = page.ID
		}
	}
	seen := make(map[string]struct{})
	links := make([]WikiLink, 0)
	warnings := make([]string, 0)
	for _, page := range pages {
		targets := append([]string(nil), page.Related...)
		for _, match := range wikilinkRE.FindAllStringSubmatch(bodies[page.ID], -1) {
			targets = append(targets, match[1])
		}
		for _, target := range targets {
			joined := filepath.Clean(filepath.Join(filepath.Dir(page.RelativePath), filepath.FromSlash(target)))
			if filepath.IsAbs(filepath.FromSlash(target)) || joined == ".." || strings.HasPrefix(joined, ".."+string(filepath.Separator)) {
				warnings = append(warnings, fmt.Sprintf("page=%s target=%s reason=path-traversal", page.ID, target))
				continue
			}
			normalized := normalizeWikiTarget(target)
			relativeTarget := normalizeWikiTarget(filepath.ToSlash(joined))
			toID := byPath[relativeTarget]
			if toID == "" {
				toID = byPath[normalized]
			}
			if toID == "" {
				toID = byName[normalized]
			}
			if toID == "" {
				warnings = append(warnings, fmt.Sprintf("page=%s target=%s", page.ID, target))
				continue
			}
			if toID == page.ID {
				continue
			}
			key := page.ID + "\x00" + toID
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			links = append(links, WikiLink{FromPageID: page.ID, ToPageID: toID, Type: "wikilink"})
		}
	}
	sort.Slice(links, func(left, right int) bool {
		if links[left].FromPageID == links[right].FromPageID {
			return links[left].ToPageID < links[right].ToPageID
		}
		return links[left].FromPageID < links[right].FromPageID
	})
	return links, warnings
}

func normalizeWikiTarget(value string) string {
	value = strings.TrimSpace(strings.TrimSuffix(filepath.ToSlash(value), ".md"))
	value = strings.TrimPrefix(value, "./")
	return strings.ToLower(value)
}

func (s *FileStore) RefreshCatalogFiles(ctx context.Context, event WikiLogEvent) ([]WikiPage, []WikiLink, []string, error) {
	pages, links, warnings, err := s.Catalog(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	indexPath, err := s.resolve("", "wiki/index.md")
	if err != nil {
		return nil, nil, nil, err
	}
	if err := atomicWrite(indexPath, []byte(renderWikiIndex(pages)), 0o640); err != nil {
		return nil, nil, nil, fmt.Errorf("llmwiki: write wiki index: %w", err)
	}
	if strings.TrimSpace(event.ID) != "" {
		if err := s.appendWikiLog(event); err != nil {
			return nil, nil, nil, err
		}
	}
	return pages, links, warnings, nil
}

func renderWikiIndex(pages []WikiPage) string {
	ordered := append([]WikiPage(nil), pages...)
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].Type != ordered[right].Type {
			return ordered[left].Type < ordered[right].Type
		}
		leftTitle := strings.ToLower(strings.TrimSpace(ordered[left].Title))
		rightTitle := strings.ToLower(strings.TrimSpace(ordered[right].Title))
		if leftTitle == rightTitle {
			return ordered[left].RelativePath < ordered[right].RelativePath
		}
		return leftTitle < rightTitle
	})
	var sources, topics strings.Builder
	for _, page := range ordered {
		var target *strings.Builder
		switch page.Type {
		case "source":
			target = &sources
		case "topic":
			target = &topics
		default:
			continue
		}
		path := strings.TrimSuffix(filepath.ToSlash(page.RelativePath), ".md")
		fmt.Fprintf(target, "- [[%s|%s]]", path, sanitizeWikiLabel(page.Title))
		if page.Description != "" {
			fmt.Fprintf(target, " — %s", sanitizeWikiLabel(page.Description))
		}
		target.WriteByte('\n')
	}
	return "# Wiki Index\n\n## Sources\n\n" + sources.String() + "\n## Topics\n\n" + topics.String()
}

func (s *FileStore) appendWikiLog(event WikiLogEvent) error {
	path, err := s.resolve("", "wiki/log.md")
	if err != nil {
		return err
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		body = []byte(initialWikiLog)
	} else if err != nil {
		return fmt.Errorf("llmwiki: read wiki log: %w", err)
	}
	marker := "<!-- event: " + sanitizeEventID(event.ID) + " -->"
	if strings.Contains(string(body), marker) {
		return nil
	}
	when := event.OccurredAt.UTC()
	if when.IsZero() {
		when = time.Now().UTC()
	}
	var entry strings.Builder
	fmt.Fprintf(&entry, "\n## %s · %s · %s\n\n%s\n", when.Format(time.RFC3339), sanitizeLogText(event.Action), sanitizeLogText(event.Subject), marker)
	if event.SourceVersionID > 0 {
		fmt.Fprintf(&entry, "- Source version: %d\n", event.SourceVersionID)
	}
	fmt.Fprintf(&entry, "- Pages: %d created, %d updated, %d deleted\n", event.CreatedPages, event.UpdatedPages, event.DeletedPages)
	if len(event.Topics) > 0 {
		entry.WriteString("- Topics: ")
		for index, topic := range event.Topics {
			if index > 0 {
				entry.WriteString(", ")
			}
			fmt.Fprintf(&entry, "[[%s|%s]]", strings.TrimSuffix(filepath.ToSlash(topic.Path), ".md"), sanitizeWikiLabel(topic.Title))
		}
		entry.WriteByte('\n')
	}
	existing := strings.TrimPrefix(string(body), initialWikiLog)
	return atomicWrite(path, []byte(initialWikiLog+entry.String()+existing), 0o640)
}

func sanitizeWikiLabel(value string) string {
	return strings.NewReplacer("\n", " ", "\r", " ", "]", "\\]").Replace(strings.TrimSpace(value))
}

func sanitizeLogText(value string) string {
	value = strings.NewReplacer("\n", " ", "\r", " ").Replace(strings.TrimSpace(value))
	if before, _, found := strings.Cut(value, "?"); found {
		value = before
	}
	return value
}

func sanitizeEventID(value string) string {
	value = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, value)
	return strings.Trim(value, "-")
}
