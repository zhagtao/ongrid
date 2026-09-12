// Package llm_wiki owns the single filesystem-scoped SQLite component used by
// the LLM Wiki bounded context.
package llm_wiki

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	biz "github.com/ongridio/ongrid/internal/manager/biz/knowledge/llm_wiki"
	"github.com/ongridio/ongrid/internal/manager/data/knowledge/llm_wiki/index"
	"github.com/ongridio/ongrid/internal/manager/data/knowledge/llm_wiki/store"
	"github.com/ongridio/ongrid/internal/pkg/dbx"
	"github.com/ongridio/ongrid/internal/pkg/embedding"
	"gorm.io/gorm"
)

type SQLiteStore struct {
	DB                    *gorm.DB
	StateRepository       biz.StateRepository
	WikiCatalogRepository biz.WikiRepository
	SearchIndex           biz.SearchIndex

	sqlDB *sql.DB
	repo  *store.Repo
}

func OpenSQLiteStore(ctx context.Context, root string, embed embedding.Embedder, dim int, log *slog.Logger) (*SQLiteStore, error) {
	if ctx == nil {
		return nil, errors.New("llmwiki sqlite: context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("llmwiki sqlite: root is required")
	}
	privateDir := filepath.Join(root, ".llm-wiki")
	if err := os.MkdirAll(privateDir, 0o750); err != nil {
		return nil, fmt.Errorf("llmwiki sqlite: create private directory: %w", err)
	}
	if err := os.Chmod(privateDir, 0o750); err != nil {
		return nil, fmt.Errorf("llmwiki sqlite: secure private directory: %w", err)
	}
	dbPath := filepath.Join(privateDir, "wiki.db")
	db, err := dbx.OpenSQLite(dbPath, log)
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("llmwiki sqlite: get connection pool: %w", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = sqlDB.Close() // the initialization error remains authoritative
		}
	}()
	if err := store.Migrate(db); err != nil {
		return nil, err
	}
	if err := os.Chmod(dbPath, 0o640); err != nil {
		return nil, fmt.Errorf("llmwiki sqlite: secure database file: %w", err)
	}
	searchIndex, err := index.New(ctx, root, db, embed, dim)
	if err != nil {
		return nil, err
	}
	repository := store.New(db)
	closeOnError = false
	return &SQLiteStore{DB: db, StateRepository: repository, WikiCatalogRepository: repository, SearchIndex: searchIndex, sqlDB: sqlDB, repo: repository}, nil
}

func (s *SQLiteStore) Repository() biz.Repository {
	if s == nil {
		return nil
	}
	return s.repo
}

func (s *SQLiteStore) Close() error {
	if s == nil || s.sqlDB == nil {
		return nil
	}
	if err := s.sqlDB.Close(); err != nil {
		return fmt.Errorf("llmwiki sqlite: close connection pool: %w", err)
	}
	return nil
}
