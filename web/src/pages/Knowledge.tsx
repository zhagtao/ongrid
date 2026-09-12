import { Input, Label, Textarea } from '@/components/ui';
import { Hint } from '@/components/ui/Tooltip';
// Knowledge page — folder-tree view of the RAG knowledge base.
//
// Layout:
//   header: title + buttons (同步内置知识库 / 刷新 / 新建文档)
//   search bar: full-width, hits a ?path_prefix scoped /knowledge/search
//   left sidebar: folder tree built from GET /knowledge/paths split on "/"
//   right pane: doc grid filtered by selected folder (path_prefix)
//
// Tree rendering: paths are flat strings like "网络/DNS"; we parse each
// into nodes and merge into a tree by walking the segments. Click any
// folder to set the path_prefix filter; click "全部" to clear.
import { useCallback, useEffect, useMemo, useRef, useState, type DragEvent, type ReactNode } from 'react';
import {
  ArrowUpRight,
  BookOpen,
  ChevronRight,
  Copy,
  DownloadCloud,
  Eye,
  Loader2,
  Folder,
  FolderOpen,
  Pencil,
  Play,
  Plus,
  RefreshCw,
  RotateCcw,
  Search,
  Trash2,
  Upload,
  X,
} from 'lucide-react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { cn } from '@/lib/cn';
import { fullDateTime } from '@/lib/format';
import { splitFrontmatter, stripFrontmatter } from '@/lib/frontmatter';
import { Modal } from '@/components/Modal';
import { Button, Card, Chip, EmptyState, PageHeader } from '@/components/ui';
import {
  compileLLMWiki,
  cancelLLMWikiJob,
  createDoc,
  deleteDoc,
  deleteLLMWikiNode,
  getLLMWikiNode,
  getLLMWikiNodePreview,
  listLLMWikiJobs,
  listLLMWikiSources,
  listLLMWikiTree,
  listDocs,
  localizedDocTitle,
  localizedPath,
  localizedPathSegment,
  moveDoc,
  retryLLMWikiJob,
  searchKnowledge,
  syncVault,
  updateDoc,
  uploadDoc,
  uploadLLMWikiFile,
  type KnowledgeDoc,
  type LLMWikiJob,
  type LLMWikiNode,
  type PathRow,
  type SearchHit,
} from '@/api/knowledge';
import { ApiError } from '@/api/client';
import { useI18n } from '@/i18n/locale';

// Drag-and-drop payload key for relocating an org doc onto a folder (ADR-029).
// Carries the doc id; folder rows in the 组织 tree are the drop targets.
const DOC_DND_MIME = 'application/x-ongrid-doc-id';

type TreeNode = {
  name: string;
  path: string; // cumulative breadcrumb (matches payload.path_prefixes)
  count: number; // doc count of this exact path
  subtreeCount: number; // doc count rolled up over the whole subtree
  children: Map<string, TreeNode>;
};

function buildTree(rows: PathRow[]): TreeNode {
  const root: TreeNode = {
    name: '__root__',
    path: '',
    count: 0,
    subtreeCount: 0,
    children: new Map(),
  };
  for (const r of rows) {
    if (!r.path) continue;
    const parts = r.path.split('/');
    let cur = root;
    let cumulative = '';
    for (let i = 0; i < parts.length; i++) {
      const seg = parts[i];
      cumulative = cumulative ? `${cumulative}/${seg}` : seg;
      let child = cur.children.get(seg);
      if (!child) {
        child = { name: seg, path: cumulative, count: 0, subtreeCount: 0, children: new Map() };
        cur.children.set(seg, child);
      }
      child.subtreeCount += r.count;
      if (i === parts.length - 1) child.count += r.count;
      cur = child;
    }
    root.subtreeCount += r.count;
  }
  return root;
}

export default function KnowledgePage() {
  const { tr } = useI18n();
  const [items, setItems] = useState<KnowledgeDoc[]>([]);
  // Two trees by source (ADR-028): 'builtin' = the read-only platform vault
  // (source_type=vault); 'org' = the organization's own content
  // (upload + manual), full CRUD. Each scope renders its own folder tree.
  const [sourceScope, setSourceScope] = useState<'builtin' | 'org' | 'wiki'>('org');
  const [wikiDirectory, setWikiDirectory] = useState<LLMWikiNode | null>(null);
  const [wikiLayer, setWikiLayer] = useState<'all' | LLMWikiNode['layer']>('all');
  const [wikiRefreshKey, setWikiRefreshKey] = useState(0);
  const [wikiUploading, setWikiUploading] = useState(false);
  const [wikiRefreshing, setWikiRefreshing] = useState(false);
  const [wikiCompileTarget, setWikiCompileTarget] = useState<string | null>(null);
  const [wikiError, setWikiError] = useState<string | null>(null);
  const [wikiDocumentCount, setWikiDocumentCount] = useState(0);
  const [viewingWikiFile, setViewingWikiFile] = useState<LLMWikiNode | null>(null);
  const [activePath, setActivePath] = useState<string>(''); // '' = 全部
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [editing, setEditing] = useState<KnowledgeDoc | 'create' | null>(null);
  const [deleting, setDeleting] = useState<KnowledgeDoc | null>(null);
  const [deletingWikiFile, setDeletingWikiFile] = useState<LLMWikiNode | null>(null);

  const [query, setQuery] = useState('');
  const [hits, setHits] = useState<SearchHit[]>([]);
  const [searching, setSearching] = useState(false);

  // Built-in vault sync state. The vault is platform content synced via the
  // dedicated /knowledge/vault/sync endpoint — it is NOT a repo row, so the
  // "同步内置知识库" button is always available and needs no repo lookup.
  // lastVaultSync caches the most recent file count for the tooltip.
  const [syncingBuiltin, setSyncingBuiltin] = useState(false);
  const [syncErr, setSyncErr] = useState<string | null>(null);
  const [lastVaultSync, setLastVaultSync] = useState<{
    count: number;
    at: string;
    source: 'cloud' | 'embedded';
  } | null>(null);
  // Transient "✓ synced" banner so a successful click has a visible result
  // (the count alone is easy to miss when it doesn't change). Cleared after
  // a few seconds or on the next action.
  const [syncOk, setSyncOk] = useState<{ count: number; source: 'cloud' | 'embedded' } | null>(null);

  const fetchAll = useCallback(async (silent = false) => {
    if (silent) setRefreshing(true);
    else setLoading(true);
    try {
      // The folder tree is derived per-scope from the docs themselves
      // (each source has its own tree), so we no longer need the aggregate
      // /knowledge/paths endpoint here.
      const docsR = await listDocs();
      setItems(docsR.items ?? []);
      setErr(null);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setLoading(false);
      setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    void fetchAll();
  }, [fetchAll]);

  const onSyncBuiltin = useCallback(async () => {
    setSyncingBuiltin(true);
    setSyncErr(null);
    setSyncOk(null);
    try {
      const res = await syncVault();
      setLastVaultSync({ count: res.file_count, at: res.synced_at, source: res.source });
      setSyncOk({ count: res.file_count, source: res.source });
      await fetchAll(true);
      window.setTimeout(() => setSyncOk(null), 6000);
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : (e as Error).message;
      setSyncErr(msg);
    } finally {
      setSyncingBuiltin(false);
    }
  }, [fetchAll]);

  // File upload (ADR-028) → 组织知识库 (source_type=upload). Accepts the
  // selected .md/.txt files, uploads each, then jumps to the org scope so
  // the operator sees what they just added.
  const [uploading, setUploading] = useState(false);
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  const wikiFileInputRef = useRef<HTMLInputElement | null>(null);
  const onUploadFiles = useCallback(
    async (files: FileList | null) => {
      if (!files || files.length === 0) return;
      setUploading(true);
      setErr(null);
      try {
        for (const f of Array.from(files)) {
          await uploadDoc(f);
        }
        setSourceScope('org');
        setActivePath('');
        await fetchAll(true);
      } catch (e) {
        setErr((e as Error).message || 'upload failed');
      } finally {
        setUploading(false);
        if (fileInputRef.current) fileInputRef.current.value = '';
      }
    },
    [fetchAll],
  );

  const refreshWiki = useCallback(async () => {
    setWikiRefreshing(true);
    setWikiError(null);
    setWikiRefreshKey((key) => key + 1);
    // Let the tree/list effects begin their next request before restoring the
    // control. The server-side reads are idempotent and require no mutation.
    await Promise.resolve();
    setWikiRefreshing(false);
  }, []);

  const uploadWikiFiles = useCallback(async (files: FileList | null) => {
    if (!files?.length) return;
    setWikiUploading(true);
    setWikiError(null);
    try {
      for (const file of Array.from(files)) await uploadLLMWikiFile(file);
      await refreshWiki();
    } catch (e) {
      setWikiError((e as Error).message);
    } finally {
      setWikiUploading(false);
      if (wikiFileInputRef.current) wikiFileInputRef.current.value = '';
    }
  }, [refreshWiki]);

  const compileWiki = useCallback(async (sourceID?: string) => {
    setWikiCompileTarget(sourceID ?? 'all');
    setWikiError(null);
    try {
      await compileLLMWiki(sourceID ? [sourceID] : []);
      await refreshWiki();
    } catch (e) {
      setWikiError((e as Error).message);
    } finally {
      setWikiCompileTarget(null);
    }
  }, [refreshWiki]);

  const isBuiltin = (d: KnowledgeDoc) => d.source_type === 'vault';

  // Both source groups are shown side-by-side in the sidebar (two trees, no
  // toggle). builtin = vault; org = everything else (upload + manual).
  const builtinItems = useMemo(() => items.filter(isBuiltin), [items]);
  const orgItems = useMemo(() => items.filter((d) => !isBuiltin(d)), [items]);

  // Each group builds its own folder tree from its own docs' paths.
  const treeFromItems = (docs: KnowledgeDoc[]): TreeNode => {
    const m = new Map<string, number>();
    for (const d of docs) {
      const p = d.path ?? '';
      if (p) m.set(p, (m.get(p) ?? 0) + 1);
    }
    const rows: PathRow[] = [...m.entries()].map(([path, count]) => ({ path, count }));
    return buildTree(rows);
  };
  const builtinTree = useMemo(() => treeFromItems(builtinItems), [builtinItems]);
  const orgTree = useMemo(() => treeFromItems(orgItems), [orgItems]);

  const scopedItems = sourceScope === 'builtin' ? builtinItems : orgItems;

  // Right pane shows docs in the selected group, filtered to the selected
  // folder (exact path OR a descendant of it).
  const visibleDocs = useMemo(() => {
    if (!activePath) return scopedItems;
    const prefix = `${activePath}/`;
    return scopedItems.filter((d) => d.path === activePath || (d.path ?? '').startsWith(prefix));
  }, [scopedItems, activePath]);

  // Picking a folder in either tree selects that group + path.
  const pickFolder = useCallback((scope: 'builtin' | 'org', path: string) => {
    setSourceScope(scope);
    setActivePath(path);
    setWikiDirectory(null);
    setWikiLayer('all');
  }, []);

  // Drag-drop: relocate an org doc into a folder (ADR-029). Optimism isn't
  // worth it here — moves re-embed server-side — so we just await + refetch.
  const onMoveDoc = useCallback(
    async (docId: string, targetPath: string) => {
      setErr(null);
      try {
        await moveDoc(docId, targetPath);
        await fetchAll(true);
      } catch (e) {
        setErr(e instanceof ApiError ? e.message : (e as Error).message);
      }
    },
    [fetchAll],
  );

  const runSearch = async () => {
    if (!query.trim()) {
      setHits([]);
      return;
    }
    setSearching(true);
    try {
      const r = await searchKnowledge(query.trim(), {
        limit: 10,
        pathPrefix: activePath || undefined,
      });
      setHits(r.items ?? []);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setSearching(false);
    }
  };

  const counts = useMemo(() => {
    let builtin = 0;
    let org = 0;
    for (const d of items) {
      if (isBuiltin(d)) builtin++;
      else org++;
    }
    return { builtin, org, total: items.length };
  }, [items]);

  return (
    <main className="anim-fade flex flex-1 flex-col overflow-hidden">
      <PageHeader
        title={tr('知识库', 'Knowledge base')}
        subtitle={
          <>
            {tr(
              `共 ${counts.total + wikiDocumentCount} 条 · 组织 ${counts.org} · 内置 ${counts.builtin} · LLM Wiki ${wikiDocumentCount}`,
              `${counts.total + wikiDocumentCount} total · ${counts.org} org · ${counts.builtin} built-in · ${wikiDocumentCount} LLM Wiki`,
            )}
            {activePath && (
              <>
                {tr(' · 当前目录 ', ' · current folder ')}
                <span className="text-zinc-300">{localizedPath(activePath)}</span>
              </>
            )}
          </>
        }
        actions={
          <>
            {/* Group-scoped actions (upload / new / sync) live on the
                sidebar section headers now; the toolbar keeps only refresh. */}
            <Input
              ref={fileInputRef}
              type="file"
              accept=".md,.markdown,.txt,.text,.pdf,.docx"
              multiple
              className="hidden"
              onChange={(e) => void onUploadFiles(e.target.files)}
            />
            <Button
              onClick={() => fetchAll(true)}
              disabled={loading || refreshing}
              variant="ghost"
            >
              <RefreshCw size={12} className={cn(refreshing && 'animate-spin')} />
              {tr('刷新', 'Refresh')}
            </Button>
          </>
        }
      />

      {syncErr && (
        <div className="mx-6 mt-3 rounded-md border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs text-amber-300">
          <div className="font-medium">
            {tr('内置知识库同步失败', 'Built-in vault sync failed')}
          </div>
          <div className="mt-0.5 break-all text-amber-200/80">{syncErr}</div>
        </div>
      )}

      {syncOk && (
        <div className="mx-6 mt-3 rounded-md border border-emerald-500/30 bg-emerald-500/10 px-3 py-2 text-xs text-emerald-300">
          {syncOk.source === 'cloud'
            ? tr(
                `✓ 已从云端同步内置知识库 · ${syncOk.count} 篇`,
                `✓ Synced built-in vault from cloud · ${syncOk.count} docs`,
              )
            : tr(
                `✓ 已同步内置知识库 · ${syncOk.count} 篇（云端不可达，使用离线内置版）`,
                `✓ Synced built-in vault · ${syncOk.count} docs (cloud unreachable — used offline baseline)`,
              )}
        </div>
      )}

      <div className="border-b border-zinc-800/60 px-6 py-2.5">
        <div className="flex items-center gap-2">
          <Label className="relative block flex-1 max-w-2xl">
            <span className="sr-only">{tr('检索', 'Search')}</span>
            <Search size={12} className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-zinc-500" />
            <Input
              type="search"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') void runSearch();
              }}
              placeholder={
                activePath
                  ? tr(`在「${activePath}」内检索（同 query_knowledge 工具）`, `Search within "${activePath}" (same as the query_knowledge tool)`)
                  : tr('试搜：LLM 看到的命中结果（同 query_knowledge 工具）', "Try a search — see what the LLM would (same as query_knowledge tool)")
              }
              className="w-full pl-8 pr-2"
            />
          </Label>
          <Button variant="primary" size="sm"
            type="button"
            onClick={() => void runSearch()}
            disabled={searching || !query.trim()}
            className="px-3 py-1.5 font-medium text-accent-fg"
          >
            {searching ? tr('检索中…', 'Searching…') : tr('检索', 'Search')}
          </Button>
          {hits.length > 0 && (
            <span className="text-[11px] text-zinc-500">{tr(`命中 ${hits.length} 条`, `${hits.length} hit(s)`)}</span>
          )}
        </div>
        {hits.length > 0 && (
          <div className="mt-3 grid gap-2">
            {hits.map((h) => (
              <SearchHitCard key={h.doc.id} hit={h} />
            ))}
          </div>
        )}
      </div>

      <div className="flex flex-1 overflow-hidden">
        <aside className="hidden w-60 shrink-0 overflow-y-auto border-r border-zinc-800 bg-zinc-950/40 px-2 py-3 md:block">
          <ScopeTreeSection
            label={tr('组织知识库', 'Organization')}
            accent="text-sky-300"
            tree={orgTree}
            total={orgItems.length}
            active={sourceScope === 'org'}
            activePath={activePath}
            onPick={(p) => pickFolder('org', p)}
            onMoveDoc={onMoveDoc}
            actions={
              <>
                <Hint content={tr('上传文件 (.md/.txt/.pdf/.docx)', 'Upload file (.md/.txt/.pdf/.docx)')}><Button variant="subtle" size="sm"
                  type="button"
                  onClick={(e) => {
                    e.stopPropagation();
                    fileInputRef.current?.click();
                  }}
                  disabled={uploading}

                  className="p-1"
                >
                  <Upload size={13} className={cn(uploading && 'animate-pulse')} />
                </Button></Hint>
                <Hint content={tr('新建文档', 'New doc')}><Button variant="subtle" size="sm"
                  type="button"
                  onClick={(e) => {
                    e.stopPropagation();
                    setSourceScope('org');
                    setEditing('create');
                  }}

                  className="p-1"
                >
                  <Plus size={13} />
                </Button></Hint>
              </>
            }
          />
          <div className="my-2 border-t border-zinc-800/60" />
          <ScopeTreeSection
            label={tr('内置知识库', 'Built-in')}
            accent="text-amber-300"
            tree={builtinTree}
            total={builtinItems.length}
            active={sourceScope === 'builtin'}
            activePath={activePath}
            onPick={(p) => pickFolder('builtin', p)}
            actions={
              <Hint content={
                  lastVaultSync
                    ? tr(
                        `上次同步 ${fullDateTime(lastVaultSync.at)} · ${lastVaultSync.count} 篇 · ${lastVaultSync.source === 'cloud' ? '来源云端' : '离线内置'}`,
                        `Last synced ${fullDateTime(lastVaultSync.at)} · ${lastVaultSync.count} docs · ${lastVaultSync.source === 'cloud' ? 'from cloud' : 'offline baseline'}`,
                      )
                    : tr('从云端同步内置知识库（github.com/ongridio/vault，连不上则用离线内置版）', 'Sync built-in vault from cloud (github.com/ongridio/vault; offline baseline if unreachable)')
                }><Button variant="subtle" size="sm"
                type="button"
                onClick={(e) => {
                  e.stopPropagation();
                  void onSyncBuiltin();
                }}
                disabled={syncingBuiltin}

                className="p-1"
              >
                <DownloadCloud size={13} className={cn(syncingBuiltin && 'animate-pulse')} />
              </Button></Hint>
            }
          />
          <div className="my-2 border-t border-zinc-800/60" />
          <LLMWikiTreeSection
            active={sourceScope === 'wiki'}
            refreshKey={wikiRefreshKey}
            onOpen={() => {
              setSourceScope('wiki');
              setActivePath('');
            }}
            onSelectDirectory={(node, layer) => {
              setWikiDirectory(node);
              setWikiLayer(layer);
            }}
            onDocumentCount={setWikiDocumentCount}
            actions={
              <>
                <input ref={wikiFileInputRef} type="file" accept=".md,.markdown,.txt,.text,.pdf,.docx" multiple className="hidden" onChange={(e) => void uploadWikiFiles(e.target.files)} />
                <button type="button" onClick={() => wikiFileInputRef.current?.click()} disabled={wikiUploading} title={tr('上传来源', 'Upload source')} className="rounded p-1 text-zinc-400 hover:bg-zinc-800 hover:text-zinc-100 disabled:opacity-50"><Upload size={13} /></button>
                <button type="button" onClick={() => void refreshWiki()} disabled={wikiRefreshing} title={tr('刷新', 'Refresh')} className="rounded p-1 text-zinc-400 hover:bg-zinc-800 hover:text-zinc-100 disabled:opacity-50"><RefreshCw size={13} className={cn(wikiRefreshing && 'animate-spin')} /></button>
                <button type="button" onClick={() => void compileWiki()} disabled={wikiCompileTarget !== null} title={tr('编译全部', 'Compile all')} className="rounded p-1 text-indigo-300 hover:bg-zinc-800 hover:text-indigo-200 disabled:opacity-50"><Play size={13} /></button>
              </>
            }
          />
          <LLMWikiJobsPanel refreshKey={wikiRefreshKey} />
        </aside>
        <div className="flex-1 overflow-y-auto px-6 py-6">
          {sourceScope === 'wiki' ? (
            <div className="h-full">
              <LLMWikiFileList
                directory={wikiDirectory}
                layer={wikiLayer}
                refreshKey={wikiRefreshKey}
                error={wikiError}
                onUpload={() => wikiFileInputRef.current?.click()}
                onRefresh={() => void refreshWiki()}
                onCompile={() => void compileWiki()}
                onCompileFile={(file) => file.source_id && void compileWiki(file.source_id)}
                onViewFile={setViewingWikiFile}
                onDeleteFile={setDeletingWikiFile}
                uploading={wikiUploading}
                refreshing={wikiRefreshing}
                compileTarget={wikiCompileTarget}
              />
            </div>
          ) : (
            <>
          {err && (
            <div className="mb-4 rounded-lg border border-red-500/40 bg-red-500/5 px-4 py-3 text-sm text-red-300">
              {err}
            </div>
          )}
          {loading ? (
            <div className="flex h-40 items-center justify-center text-sm text-zinc-500">{tr('加载中…', 'Loading…')}</div>
          ) : visibleDocs.length === 0 ? (
            <EmptyState
              icon={BookOpen}
              title={
                activePath
                  ? tr(`「${activePath}」里还没有文档`, `No docs in "${activePath}" yet`)
                  : sourceScope === 'builtin'
                    ? tr('内置知识库为空', 'Built-in knowledge base is empty')
                    : tr('组织知识库还没有内容', 'No organization docs yet')
              }
              action={
                sourceScope === 'builtin' ? (
                  <Button variant="ghost" onClick={() => void onSyncBuiltin()} disabled={syncingBuiltin}>
                    <DownloadCloud size={12} /> {tr('从云端同步内置知识库', 'Sync built-in vault from cloud')}
                  </Button>
                ) : (
                  <div className="flex items-center gap-2">
                    <Button variant="ghost" onClick={() => fileInputRef.current?.click()} disabled={uploading}>
                      <Upload size={12} /> {tr('上传文件', 'Upload')}
                    </Button>
                    <Button variant="primary" onClick={() => setEditing('create')}>
                      <Plus size={12} /> {tr('新建文档', 'New doc')}
                    </Button>
                  </div>
                )
              }
            />
          ) : (
            <div className="flex flex-col gap-1.5">
              {visibleDocs.map((d) => (
                <DocCard
                  key={d.id}
                  doc={d}
                  onEdit={() => setEditing(d)}
                  onDelete={() => setDeleting(d)}
                />
              ))}
            </div>
          )}
            </>
          )}
        </div>
      </div>

      {editing && (
        <DocEditor
          mode={editing === 'create' ? 'create' : 'edit'}
          existing={editing === 'create' ? null : editing}
          defaultPath={editing === 'create' ? activePath : undefined}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            void fetchAll(true);
          }}
        />
      )}
      {viewingWikiFile && (
        <LLMWikiFileViewer
          file={viewingWikiFile}
          onClose={() => setViewingWikiFile(null)}
          onOpenFile={setViewingWikiFile}
          onDelete={() => {
            setViewingWikiFile(null);
            setDeletingWikiFile(viewingWikiFile);
          }}
        />
      )}
      {deletingWikiFile && (
        <DeleteLLMWikiFileDialog
          file={deletingWikiFile}
          onClose={() => setDeletingWikiFile(null)}
          onDone={() => {
            setDeletingWikiFile(null);
            void refreshWiki();
          }}
        />
      )}
      {deleting && (
        <DeleteDocDialog
          doc={deleting}
          onClose={() => setDeleting(null)}
          onDone={() => {
            setDeleting(null);
            void fetchAll(true);
          }}
        />
      )}
    </main>
  );
}

function LLMWikiJobsPanel({ refreshKey }: { refreshKey: number }) {
  const { tr } = useI18n();
  const [jobs, setJobs] = useState<LLMWikiJob[]>([]);
  const [sourceNames, setSourceNames] = useState<Record<string, string>>({});
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [actionID, setActionID] = useState<string | null>(null);

  const loadJobs = useCallback(async (silent = false) => {
    if (!silent) setLoading(true);
    try {
      const [result, sources] = await Promise.all([
        listLLMWikiJobs(),
        listLLMWikiSources().catch(() => ({ items: [] as { id: string; raw_path: string }[], total: 0 })),
      ]);
      setJobs(result.items ?? []);
      setSourceNames(Object.fromEntries(sources.items.map((source) => [source.id, source.raw_path])));
      setError(null);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      if (!silent) setLoading(false);
    }
  }, []);

  useEffect(() => {
    void loadJobs();
  }, [loadJobs, refreshKey]);

  useEffect(() => {
    const hasActiveJob = jobs.some((job) => job.status === 'pending' || job.status === 'running');
    if (!hasActiveJob) return undefined;
    const timer = window.setInterval(() => void loadJobs(true), 3000);
    return () => window.clearInterval(timer);
  }, [jobs, loadJobs]);

  const retry = async (id: string) => {
    setActionID(id);
    setError(null);
    try {
      await retryLLMWikiJob(id);
      await loadJobs();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setActionID(null);
    }
  };

  const cancel = async (id: string) => {
    setActionID(id);
    setError(null);
    try {
      await cancelLLMWikiJob(id);
      await loadJobs();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setActionID(null);
    }
  };

  const visibleJobs = jobs.filter((job) => job.status === 'pending' || job.status === 'running' || job.status === 'failed');
  if (!loading && visibleJobs.length === 0 && !error) return null;

  return (
    <Card compact className="mx-1 mt-3 p-2.5">
      <div className="mb-2 flex items-center justify-between gap-2">
        <div>
          <div className="text-xs font-medium text-zinc-100">{tr('编译任务', 'Compile jobs')}</div>
        </div>
        <button type="button" aria-label={tr('刷新编译任务', 'Refresh compile jobs')} onClick={() => void loadJobs()} disabled={loading} title={tr('刷新编译任务', 'Refresh compile jobs')} className="rounded p-1 text-zinc-500 hover:bg-zinc-800 hover:text-zinc-200 disabled:opacity-50">
          <RefreshCw size={12} className={cn(loading && 'animate-spin')} />
        </button>
      </div>
      {error && <div className="mb-2 rounded border border-red-500/30 bg-red-500/5 px-2 py-1.5 text-xs text-red-300">{error}</div>}
      {loading && jobs.length === 0 ? (
        <div className="text-xs text-zinc-500">{tr('加载任务中…', 'Loading jobs…')}</div>
      ) : (
        <div className="divide-y divide-zinc-800/60">
          {jobs.map((job) => {
            const active = job.status === 'pending' || job.status === 'running';
            const failed = job.status === 'failed';
            if (!active && !failed) return null;
            const statusLabel = job.status === 'pending'
              ? tr('排队中', 'Pending')
              : job.status === 'running'
                ? tr('编译中', 'Running')
                : job.status === 'succeeded'
                  ? tr('成功', 'Succeeded')
                  : job.status === 'skipped'
                    ? tr('已跳过', 'Skipped')
                    : job.status === 'cancelled'
                      ? tr('已取消', 'Cancelled')
                      : tr('失败', 'Failed');
            const statusClass = failed
              ? 'text-red-300'
              : active
                ? 'text-amber-300'
                : 'text-zinc-400';
            return (
              <div key={job.id} className="flex items-start justify-between gap-2 py-2 first:pt-0 last:pb-0">
                <div className="min-w-0">
                  <div className="flex flex-wrap items-center gap-2 text-xs">
                    <span className={cn('font-medium', statusClass)}>{statusLabel}</span>
                    <span className="truncate text-zinc-500">{tr('阶段', 'Stage')}: {job.stage || 'queued'}</span>
                  </div>
                  <div className="mt-0.5 truncate text-[11px] text-zinc-300" title={job.source_ids.map((id) => sourceNames[id] ?? id).join('、')}>
                    {job.source_ids.map((id) => sourceNames[id] ?? id).join('、') || tr('未知文件', 'Unknown file')}
                  </div>
                  {job.error && <div className="mt-1 break-words text-xs text-red-300">{job.error}</div>}
                </div>
                {active ? (
                  <button type="button" aria-label={tr('取消编译', 'Cancel compile')} title={tr('取消编译', 'Cancel compile')} className="shrink-0 rounded p-1 text-zinc-400 hover:bg-zinc-800 hover:text-zinc-100 disabled:opacity-50" onClick={() => void cancel(job.id)} disabled={actionID === job.id}>
                    <X size={13} className={cn(actionID === job.id && 'animate-spin')} />
                  </button>
                ) : (
                  <button type="button" aria-label={tr('重试编译', 'Retry compile')} title={tr('重试编译', 'Retry compile')} className="shrink-0 rounded p-1 text-zinc-400 hover:bg-zinc-800 hover:text-zinc-100 disabled:opacity-50" onClick={() => void retry(job.id)} disabled={actionID === job.id}>
                    {actionID === job.id ? <Loader2 size={12} className="animate-spin" /> : <RotateCcw size={12} />}
                  </button>
                )}
              </div>
            );
          })}
        </div>
      )}
    </Card>
  );
}

function LLMWikiFileList({
  directory,
  layer,
  refreshKey,
  error,
  onUpload,
  onRefresh,
  onCompile,
  onCompileFile,
  onViewFile,
  onDeleteFile,
  uploading,
  refreshing,
  compileTarget,
}: {
  directory: LLMWikiNode | null;
  layer: 'all' | LLMWikiNode['layer'];
  refreshKey: number;
  error: string | null;
  onUpload: () => void;
  onRefresh: () => void;
  onCompile: () => void;
  onCompileFile: (file: LLMWikiNode) => void;
  onViewFile: (file: LLMWikiNode) => void;
  onDeleteFile: (file: LLMWikiNode) => void;
  uploading: boolean;
  refreshing: boolean;
  compileTarget: string | null;
}) {
  const { tr } = useI18n();
  const [items, setItems] = useState<LLMWikiNode[]>([]);
  const [loading, setLoading] = useState(true);
  const [listError, setListError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      setLoading(true);
      try {
        // The Wiki root is an "all files" view. A folder selection stays
        // scoped to that directory, while root collects every generated page.
        const files = directory
          ? (await listLLMWikiTree(directory.layer, directory.id)).items.filter((item) => item.kind === 'file')
          : layer === 'all'
            ? await listAllLLMWikiFiles('wiki')
            : layer === 'raw' || layer === 'wiki'
              ? await listAllLLMWikiFiles(layer)
              : (await listLLMWikiTree(layer)).items.filter((item) => item.kind === 'file');
        if (!cancelled) {
          setItems(files);
          setListError(null);
        }
      } catch (e) {
        if (!cancelled) setListError((e as Error).message);
      } finally {
        if (!cancelled) setLoading(false);
      }
    };
    void load();
    return () => { cancelled = true; };
  }, [directory, layer, refreshKey]);

  const visibleItems = items;

  const actions = (
    <div className="flex flex-wrap items-center justify-center gap-2">
      <Button variant="ghost" onClick={onUpload} disabled={uploading}><Upload size={12} /> {uploading ? tr('上传中…', 'Uploading…') : tr('上传来源', 'Upload source')}</Button>
      <Button variant="ghost" onClick={onRefresh} disabled={refreshing}><RefreshCw size={12} className={cn(refreshing && 'animate-spin')} /> {tr('刷新', 'Refresh')}</Button>
      <Button variant="primary" onClick={onCompile} disabled={compileTarget !== null}><Play size={12} /> {compileTarget === 'all' ? tr('创建中…', 'Creating…') : tr('编译全部', 'Compile all')}</Button>
    </div>
  );

  if (loading) return <div className="flex h-40 items-center justify-center text-sm text-zinc-500">{tr('加载中…', 'Loading…')}</div>;
  if (error || listError) return <div className="rounded-md border border-red-500/30 bg-red-500/5 px-3 py-2 text-xs text-red-300">{error ?? listError}</div>;
  if (items.length === 0 && !directory && layer === 'all') {
    return <EmptyState
      icon={BookOpen}
      title={tr('LLM Wiki 还没有内容', 'LLM Wiki has no content yet')}
      action={actions}
    />;
  }
  return <div className="space-y-3">
    {visibleItems.length === 0 ? (
      <EmptyState icon={BookOpen} title={tr('此目录暂无文件', 'No files in this directory')} />
    ) : (
      <div className="flex flex-col gap-1.5">{visibleItems.map((file) => (
    <Card key={file.id} compact onClick={() => onViewFile(file)} className="flex cursor-pointer flex-col py-2.5 transition-colors hover:bg-zinc-800/40">
      <div className="flex items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="truncate text-sm font-medium text-zinc-100">{file.layer === 'raw' ? file.name : file.name.replace(/\.md$/i, '')}</div>
          <div className="mt-0.5 flex items-center gap-1.5 text-[10px] uppercase tracking-wider text-zinc-500">
            <span className={file.layer === 'raw' ? 'text-amber-300' : file.page_type === 'topic' ? 'text-indigo-300' : 'text-sky-300'}>{file.layer === 'raw' ? 'Raw' : file.page_type === 'topic' ? 'Topic' : 'Source'}</span>
            <span className="rounded bg-zinc-800/60 px-1.5 py-0.5 font-normal normal-case text-zinc-300">{directory?.name ?? (file.layer === 'raw' ? 'Raw' : layer === 'all' ? tr('全部文件', 'All files') : layer === 'wiki' ? 'Wiki' : 'schema.md')}</span>
          </div>
          <div className="mt-1 truncate font-mono text-[10px] text-zinc-600">{file.relative_path}</div>
        </div>
        <div className="flex shrink-0 items-center gap-1">
          <Eye size={15} className="text-zinc-500" />
          {file.layer === 'raw' && file.source_id && (
            <button
              type="button"
              onClick={(event) => {
                event.stopPropagation();
                onCompileFile(file);
              }}
              disabled={compileTarget !== null}
              aria-label={tr(`编译 ${file.name}`, `Compile ${file.name}`)}
              title={tr('编译此来源', 'Compile this source')}
              className="rounded p-1 text-indigo-300 hover:bg-zinc-800 hover:text-indigo-200 disabled:opacity-50"
            >
              {compileTarget === file.source_id ? <Loader2 size={13} className="animate-spin" /> : <Play size={13} />}
            </button>
          )}
          {(file.layer === 'raw' || file.page_type === 'topic') && (
            <button
              type="button"
              onClick={(event) => {
                event.stopPropagation();
                onDeleteFile(file);
              }}
              aria-label={tr(`删除 ${file.name}`, `Delete ${file.name}`)}
              title={file.layer === 'raw' ? tr('删除 Raw 文件', 'Delete raw file') : tr('删除 Wiki 文件', 'Delete Wiki file')}
              className="rounded p-1 text-zinc-500 hover:bg-red-900/30 hover:text-red-300"
            >
              <Trash2 size={13} />
            </button>
          )}
        </div>
      </div>
    </Card>
      ))}</div>
    )}
  </div>;
}

async function listAllLLMWikiFiles(layer: 'raw' | 'wiki' = 'wiki', parentID?: string): Promise<LLMWikiNode[]> {
  const result = await listLLMWikiTree(layer, parentID);
  const entries = result.items ?? [];
  const descendants = await Promise.all(
    entries.filter((entry) => entry.kind === 'folder').map((folder) => listAllLLMWikiFiles(layer, folder.id)),
  );
  const files = [...entries.filter((entry) => entry.kind === 'file'), ...descendants.flat()];
  return files;
}

function LLMWikiFileViewer({ file, onClose, onOpenFile, onDelete }: { file: LLMWikiNode; onClose: () => void; onOpenFile: (file: LLMWikiNode) => void; onDelete: () => void }) {
  const { tr } = useI18n();
  const [detail, setDetail] = useState<LLMWikiNode | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [preview, setPreview] = useState<LLMWikiRawPreview | null>(null);
  const [previewError, setPreviewError] = useState<string | null>(null);
  const previewKind = rawPreviewKind(file);

  useEffect(() => {
    let cancelled = false;
    setDetail(null);
    setError(null);
    void getLLMWikiNode(file.id)
      .then((node) => { if (!cancelled) setDetail(node); })
      .catch((reason: unknown) => { if (!cancelled) setError((reason as Error).message); });
    return () => { cancelled = true; };
  }, [file.id]);

  useEffect(() => {
    let cancelled = false;
    let objectURL: string | null = null;
    setPreview(null);
    setPreviewError(null);
    if (!previewKind) return () => { cancelled = true; };
    void getLLMWikiNodePreview(file.id)
      .then(async (blob) => {
        if (cancelled) return;
        if (previewKind === 'pdf') {
          objectURL = URL.createObjectURL(blob);
          setPreview({ kind: 'pdf', url: objectURL });
        } else {
          setPreview({ kind: 'docx', text: await blob.text() });
        }
      })
      .catch((reason: unknown) => { if (!cancelled) setPreviewError((reason as Error).message); });
    return () => {
      cancelled = true;
      if (objectURL) URL.revokeObjectURL(objectURL);
    };
  }, [file.id, previewKind]);

  return (
    <Modal
      open
      onClose={onClose}
      title={file.name}
      size="xl"
      resizable
      footer={
        <div className="flex items-center justify-end gap-2">
          {(file.layer === 'raw' || file.page_type === 'topic') && (
            <button type="button" onClick={onDelete} className="rounded-md border border-red-500/40 bg-red-500/10 px-3 py-1.5 text-xs text-red-300 hover:bg-red-500/20">
              <Trash2 size={12} className="mr-1 inline" />{tr('删除文件', 'Delete file')}
            </button>
          )}
          <button type="button" onClick={onClose} className="rounded-md border border-zinc-700 bg-zinc-900 px-3 py-1.5 text-xs text-zinc-300 hover:bg-zinc-800">{tr('关闭', 'Close')}</button>
        </div>
      }
    >
      <div className="space-y-3">
        <div className="font-mono text-[11px] text-zinc-500">{file.relative_path}</div>
        {detail && <div className="text-xs text-zinc-500">{tr('更新时间', 'Updated')}：{fullDateTime(detail.updated_at)}</div>}
        {detail?.layer === 'raw' ? (
          <LLMWikiReferences
            title={tr(`关联 (${detail.metadata?.wiki_files?.length ?? 0})`, `Related (${detail.metadata?.wiki_files?.length ?? 0})`)}
            references={detail.metadata?.wiki_files}
            emptyLabel={tr('暂无关联 Wiki', 'No related Wiki pages')}
            layer="wiki"
            onOpenFile={onOpenFile}
          />
        ) : (
          detail && <div className="space-y-3">
            <LLMWikiTopicMetadata metadata={detail.metadata} />
            <LLMWikiSourceReferences metadata={detail.metadata} onOpenFile={onOpenFile} />
          </div>
        )}
        <div className="rounded-md border border-zinc-800 bg-zinc-950/40 p-4">
          {error ? <div className="text-xs text-red-300">{error}</div> : detail ? previewKind ? <LLMWikiRawPreview kind={previewKind} preview={preview} error={previewError} fileName={file.name} tr={tr} /> : <DocBody content={detail.content ?? ''} omitMetaKeys={detail.layer === 'wiki' ? ['id', 'type', 'language', 'source_versions', 'title', 'source_file'] : undefined} /> : <div className="text-xs text-zinc-500">{tr('加载中…', 'Loading…')}</div>}
        </div>
      </div>
    </Modal>
  );
}

function LLMWikiTopicMetadata({ metadata }: { metadata?: LLMWikiNode['metadata'] }) {
  const { tr } = useI18n();
  if (metadata?.page_type !== 'topic') return null;
  const groups = [
    { label: tr('别名', 'Aliases'), values: metadata.aliases },
    { label: tr('相关实体', 'Related Entities'), values: metadata.entities },
    { label: tr('相关概念', 'Related Concepts'), values: metadata.concepts },
  ];
  return (
    <Card compact className="space-y-3">
      <Chip tone="info" dense>Topic</Chip>
      {groups.map(({ label, values }) => (
        <div key={label}>
          <div className="mb-1.5 text-[11px] font-medium text-zinc-400">{label}</div>
          {values?.length ? (
            <div className="flex flex-wrap gap-1.5">
              {values.map((value) => <Chip key={value} dense>{value}</Chip>)}
            </div>
          ) : (
            <div className="text-xs text-zinc-500">{tr('暂无', 'None')}</div>
          )}
        </div>
      ))}
    </Card>
  );
}

type LLMWikiRawPreview =
  | { kind: 'pdf'; url: string }
  | { kind: 'docx'; text: string };

function rawPreviewKind(file: LLMWikiNode): 'pdf' | 'docx' | null {
  if (file.layer !== 'raw') return null;
  const extension = file.relative_path.toLowerCase().split('.').pop();
  return extension === 'pdf' || extension === 'docx' ? extension : null;
}

function LLMWikiRawPreview({ kind, preview, error, fileName, tr }: { kind: 'pdf' | 'docx'; preview: LLMWikiRawPreview | null; error: string | null; fileName: string; tr: (zh: string, en: string) => string }) {
  if (error) return <div className="text-xs text-red-300">{error}</div>;
  if (!preview) return <div className="text-xs text-zinc-500">{tr('加载预览…', 'Loading preview…')}</div>;
  if (kind === 'pdf' && preview.kind === 'pdf') {
    return <iframe title={fileName} src={preview.url} className="h-[min(70vh,720px)] w-full rounded border border-zinc-800 bg-white" />;
  }
  if (kind === 'docx' && preview.kind === 'docx') {
    return <pre className="max-h-[min(70vh,720px)] overflow-auto whitespace-pre-wrap text-sm leading-6 text-zinc-300">{preview.text}</pre>;
  }
  return null;
}

type LLMWikiFileReference = {
  path: string;
  node_id?: string;
  title?: string;
};

function LLMWikiReferences({ title, references, emptyLabel, layer, onOpenFile }: { title: string; references?: LLMWikiFileReference[]; emptyLabel: string; layer: LLMWikiNode['layer']; onOpenFile: (file: LLMWikiNode) => void }) {
  return (
    <div className="rounded-md border border-zinc-800 bg-zinc-950/40 px-3 py-2">
      <div className="mb-1 text-[11px] font-medium text-zinc-400">{title}</div>
      {references?.length ? (
        <div className="flex flex-wrap gap-1.5">
          {references.map((reference) => (
            <LLMWikiFileReferenceButton key={reference.node_id ?? reference.path} reference={reference} layer={layer} onOpenFile={onOpenFile} />
          ))}
        </div>
      ) : (
        <div className="text-xs text-zinc-500">{emptyLabel}</div>
      )}
    </div>
  );
}

function LLMWikiFileReferenceButton({ reference, layer, onOpenFile }: { reference: LLMWikiFileReference; layer: LLMWikiNode['layer']; onOpenFile: (file: LLMWikiNode) => void }) {
  const target = toLLMWikiFileReference(reference, layer);
  const content = (
    <>
      <span className="truncate text-zinc-300">{reference.title || reference.path}</span>
      {reference.title && <span className="truncate font-mono text-[11px] text-zinc-500">{reference.path}</span>}
      {target && <ArrowUpRight size={12} className="shrink-0 text-zinc-500" />}
    </>
  );
  if (!target) return <div className="flex min-w-0 items-center gap-1.5 text-xs">{content}</div>;
  return (
    <button type="button" onClick={() => onOpenFile(target)} aria-label={reference.path} className="flex min-w-0 max-w-full shrink-0 items-center gap-1.5 rounded-full border border-zinc-800 px-2 py-1 text-left text-xs hover:border-zinc-600 hover:bg-zinc-900">
      {content}
    </button>
  );
}

function toLLMWikiFileReference(reference: LLMWikiFileReference, layer: LLMWikiNode['layer']): LLMWikiNode | null {
  if (!reference.node_id) return null;
  return {
    id: reference.node_id,
    parent_id: '',
    layer,
    kind: 'file',
    name: reference.path.split('/').pop() || reference.path,
    relative_path: reference.path,
    has_children: false,
    child_count: 0,
    document_count: 1,
  };
}

function LLMWikiSourceReferences({ metadata, onOpenFile }: { metadata?: LLMWikiNode['metadata']; onOpenFile: (file: LLMWikiNode) => void }) {
  const { tr } = useI18n();
  const references: LLMWikiFileReference[] = metadata?.source_files?.length
    ? metadata.source_files.map(({ path, node_id }) => ({ path, node_id }))
    : metadata?.source_file
      ? [{ path: metadata.source_file, node_id: metadata.source_node_id }]
      : [];
  if (references.length === 0) return null;
  return <LLMWikiReferences title={tr(`来源 (${references.length})`, `Sources (${references.length})`)} references={references} emptyLabel={tr('暂无关联 Source', 'No related sources')} layer="raw" onOpenFile={onOpenFile} />;
}

// LLMWikiTreeSection keeps the Wiki hierarchy in the same navigation shape
// as the organization and built-in knowledge trees. Children are fetched only
// when an operator expands a folder, so deep Wiki libraries do not inflate the
// first knowledge-page request.
function LLMWikiTreeSection({
  active,
  refreshKey,
  onOpen,
  onSelectDirectory,
  onDocumentCount,
  actions,
}: {
  active: boolean;
  refreshKey: number;
  onOpen: () => void;
  onSelectDirectory: (node: LLMWikiNode | null, layer: LLMWikiNode['layer'] | 'all') => void;
  onDocumentCount: (count: number) => void;
  actions: ReactNode;
}) {
  const { tr } = useI18n();
  const [nodes, setNodes] = useState<Record<string, LLMWikiNode[]>>({});
  const [expanded, setExpanded] = useState<Set<string>>(new Set(['root']));
  const [documentCount, setDocumentCount] = useState(0);

  const load = useCallback(async (layer: LLMWikiNode['layer'], parentID = '') => {
    try {
      const result = await listLLMWikiTree(layer, parentID || undefined);
      setNodes((current) => ({ ...current, [`${layer}:${parentID}`]: result.items ?? [] }));
      if (layer === 'wiki' && !parentID) {
        const count = result.document_count ?? 0;
        setDocumentCount(count);
        onDocumentCount(count);
      }
    } catch {
      // The LLM Wiki worker may be disabled on an otherwise healthy knowledge
      // page. Leave this optional tree empty instead of breaking the sidebar.
    }
  }, [onDocumentCount]);

  useEffect(() => { void Promise.all([load('raw'), load('wiki'), load('schema')]); }, [load, refreshKey]);

  const toggle = async (node: LLMWikiNode) => {
    onOpen();
    if (node.kind !== 'folder') return;
    if (node.id === `${node.layer}:root`) {
      setExpanded((current) => {
        const next = new Set(current);
        if (next.has(node.id)) next.delete(node.id); else next.add(node.id);
        return next;
      });
      return;
    }
    if (expanded.has(node.id)) {
      setExpanded((current) => { const next = new Set(current); next.delete(node.id); return next; });
      return;
    }
    if (!nodes[`${node.layer}:${node.id}`]) await load(node.layer, node.id);
    setExpanded((current) => new Set(current).add(node.id));
  };

  return (
    <div>
      <div className="flex items-center gap-1">
        <button
          type="button"
          onClick={() => {
            onSelectDirectory(null, 'all');
            onOpen();
          }}
          className={cn(
            'flex min-w-0 flex-1 items-center justify-between gap-2 rounded-md px-2 py-1.5 text-xs',
            active ? 'bg-zinc-800' : 'hover:bg-zinc-900',
          )}
        >
          <span className="flex items-center gap-1.5 font-semibold text-indigo-300">
            <BookOpen size={12} /> LLM Wiki
          </span>
          {documentCount > 0 && <span className="text-[10px] text-zinc-500">{documentCount}</span>}
        </button>
        <div className="flex shrink-0 items-center gap-0.5 pr-1">{actions}</div>
      </div>
      <div className="mt-0.5">
        <LLMWikiLayerRoot
          label={tr('原始来源', 'Raw sources')}
          layer="raw"
          nodes={nodes}
          expanded={expanded}
          onToggle={toggle}
          onSelectDirectory={onSelectDirectory}
          onOpen={onOpen}
        />
        <LLMWikiLayerRoot
          label={tr('生成页面', 'Generated pages')}
          layer="wiki"
          nodes={nodes}
          expanded={expanded}
          onToggle={toggle}
          onSelectDirectory={onSelectDirectory}
          onOpen={onOpen}
        />
      </div>
    </div>
  );
}

function LLMWikiLayerRoot({ label, layer, nodes, expanded, onToggle, onSelectDirectory, onOpen }: {
  label: string; layer: 'raw' | 'wiki'; nodes: Record<string, LLMWikiNode[]>; expanded: Set<string>; onToggle: (node: LLMWikiNode) => void; onSelectDirectory: (node: LLMWikiNode | null, layer: LLMWikiNode['layer'] | 'all') => void; onOpen: () => void;
}) {
  const id = `${layer}:root`;
  const open = expanded.has(id);
  const documentCount = (nodes[`${layer}:`] ?? []).reduce((total, node) => total + node.document_count, 0);
  const root: LLMWikiNode = { id, parent_id: '', layer, kind: 'folder', name: label, relative_path: '', has_children: true, child_count: 0, document_count: 0 };
  return <div>
    <div className="flex items-center gap-1 rounded-md px-1.5 py-1 text-xs text-zinc-400 hover:bg-zinc-900 hover:text-zinc-200">
      <button type="button" onClick={() => void onToggle(root)} className="flex h-4 w-4 items-center justify-center text-zinc-500" aria-label={open ? 'Collapse' : 'Expand'}><ChevronRight size={11} className={cn(open && 'rotate-90')} /></button>
      <button type="button" onClick={() => { void onToggle(root); onSelectDirectory(null, layer); onOpen(); }} className="flex min-w-0 flex-1 items-center justify-between gap-1.5 text-left"><span className="flex min-w-0 items-center gap-1.5">{open ? <FolderOpen size={12} /> : <Folder size={12} />}<span>{label}</span></span>{documentCount > 0 && <span className="text-[10px] text-zinc-500">{documentCount}</span>}</button>
    </div>
    {open && <LLMWikiTreeNodes nodes={nodes} layer={layer} parentID="" depth={1} expanded={expanded} onToggle={onToggle} onSelectDirectory={(node) => { onSelectDirectory(node, layer); onOpen(); }} />}
  </div>;
}

function LLMWikiTreeNodes({
  nodes,
  layer,
  parentID,
  depth,
  expanded,
  onToggle,
  onSelectDirectory,
  emptyLabel,
}: {
  nodes: Record<string, LLMWikiNode[]>;
  layer: 'raw' | 'wiki';
  parentID: string;
  depth: number;
  expanded: Set<string>;
  onToggle: (node: LLMWikiNode) => void;
  onSelectDirectory: (node: LLMWikiNode) => void;
  emptyLabel?: string;
}) {
  const items = nodes[`${layer}:${parentID}`];
  if (!items) return null;
  if (items.length === 0 && parentID === '') {
    return <div className="px-2 py-1 text-[10px] text-zinc-500">{emptyLabel}</div>;
  }
  return (
    <>
      {items.map((node) => {
        if (node.kind === 'file') return null;
        const isOpen = expanded.has(node.id);
        const Icon = isOpen ? FolderOpen : Folder;
        return (
          <div key={node.id}>
            <div
              className="flex items-center gap-1 rounded-md px-1.5 py-1 text-xs text-zinc-400 hover:bg-zinc-900 hover:text-zinc-200"
              style={{ paddingLeft: 6 + depth * 12 }}
            >
              <button
                type="button"
                onClick={() => void onToggle(node)}
                className="flex h-4 w-4 shrink-0 items-center justify-center text-zinc-500"
                aria-label={isOpen ? 'Collapse' : 'Expand'}
              >
                <ChevronRight size={11} className={cn(isOpen && 'rotate-90')} />
              </button>
              <button
                type="button"
                onClick={() => {
                  void onToggle(node);
                  onSelectDirectory(node);
                }}
                className="flex min-w-0 flex-1 items-center justify-between gap-1.5 text-left"
              >
                <span className="flex min-w-0 items-center gap-1.5"><Icon size={12} className="shrink-0" /><span className="truncate">{node.name}</span></span>
                {node.document_count > 0 && <span className="text-[10px] text-zinc-500">{node.document_count}</span>}
              </button>
            </div>
            {isOpen && <LLMWikiTreeNodes nodes={nodes} layer={layer} parentID={node.id} depth={depth + 1} expanded={expanded} onToggle={onToggle} onSelectDirectory={onSelectDirectory} />}
          </div>
        );
      })}
    </>
  );
}

// ScopeTreeSection renders one labeled source group's folder tree (no own
// <aside> — the parent stacks two of these). Highlights are shown only when
// this section's scope is the active one; the other section stays visible
// and browsable but unhighlighted.
function ScopeTreeSection({
  label,
  accent,
  tree,
  total,
  active,
  activePath,
  onPick,
  onMoveDoc,
  actions,
}: {
  label: string;
  accent: string;
  tree: TreeNode;
  total: number;
  active: boolean;
  activePath: string;
  onPick: (p: string) => void;
  // When set (org tree only), folders + the group root accept dropped doc
  // cards to relocate them (ADR-029). Built-in tree leaves this undefined.
  onMoveDoc?: (docId: string, path: string) => void;
  // Group-scoped action icons (upload/new for org, sync for built-in)
  // rendered on the right of the header.
  actions?: ReactNode;
}) {
  // Feed FolderNode a path that matches nothing when this scope isn't
  // active, so only the selected tree shows a highlight.
  const childActivePath = active ? activePath : '';
  const [rootDragOver, setRootDragOver] = useState(false);
  const acceptDrop = (e: DragEvent) => {
    if (!onMoveDoc || !e.dataTransfer.types.includes(DOC_DND_MIME)) return false;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    return true;
  };
  return (
    <div>
      {/* The section header IS the group root — clicking it shows the whole
          group (no separate "全部" row); folders are the next level down.
          It's also the drop target for "move to root". */}
      <div className="flex items-center gap-1">
        <Button variant="subtle" size="sm"
          type="button"
          onClick={() => onPick('')}
          onDragOver={onMoveDoc ? (e) => setRootDragOver(acceptDrop(e)) : undefined}
          onDragLeave={onMoveDoc ? () => setRootDragOver(false) : undefined}
          onDrop={
            onMoveDoc
              ? (e) => {
                  setRootDragOver(false);
                  const id = e.dataTransfer.getData(DOC_DND_MIME);
                  if (id) onMoveDoc(id, '');
                }
              : undefined
          }
          className={cn(
            'flex min-w-0 flex-1 items-center justify-between gap-2 rounded-md px-2 py-1.5 text-xs',
            rootDragOver
              ? 'ring-1 ring-sky-400/70 bg-sky-500/10'
              : active && activePath === ''
                ? 'bg-zinc-800'
                : 'hover:bg-zinc-900',
          )}
        >
          <span className={cn('flex items-center gap-1.5 truncate font-semibold', accent)}>
            <BookOpen size={12} />
            {label}
          </span>
          <span className="text-[10px] text-zinc-500">{total}</span>
        </Button>
        {actions && <div className="flex shrink-0 items-center gap-0.5 pr-1">{actions}</div>}
      </div>
      <div className="mt-0.5">
        {[...tree.children.values()]
          .sort((a, b) => a.name.localeCompare(b.name))
          .map((n) => (
            <FolderNode key={n.path} node={n} depth={0} activePath={childActivePath} onPick={onPick} onMoveDoc={onMoveDoc} />
          ))}
      </div>
    </div>
  );
}

function FolderNode({
  node,
  depth,
  activePath,
  onPick,
  onMoveDoc,
}: {
  node: TreeNode;
  depth: number;
  activePath: string;
  onPick: (p: string) => void;
  onMoveDoc?: (docId: string, path: string) => void;
}) {
  const { tr } = useI18n();
  const isActive = activePath === node.path;
  const isAncestor = activePath.startsWith(`${node.path}/`);
  const [expanded, setExpanded] = useState(isActive || isAncestor);
  const [dragOver, setDragOver] = useState(false);
  useEffect(() => {
    if (isActive || isAncestor) setExpanded(true);
  }, [isActive, isAncestor]);

  const hasChildren = node.children.size > 0;
  const Icon = expanded ? FolderOpen : Folder;
  const canDrop = (e: DragEvent) =>
    !!onMoveDoc && e.dataTransfer.types.includes(DOC_DND_MIME);

  return (
    <div>
      <div
        onDragOver={
          onMoveDoc
            ? (e) => {
                if (canDrop(e)) {
                  e.preventDefault();
                  e.dataTransfer.dropEffect = 'move';
                  setDragOver(true);
                }
              }
            : undefined
        }
        onDragLeave={onMoveDoc ? () => setDragOver(false) : undefined}
        onDrop={
          onMoveDoc
            ? (e) => {
                setDragOver(false);
                if (!canDrop(e)) return;
                e.preventDefault();
                const id = e.dataTransfer.getData(DOC_DND_MIME);
                if (id) onMoveDoc(id, node.path);
              }
            : undefined
        }
        className={cn(
          'group flex items-center gap-1 rounded-md px-1.5 py-1 text-xs',
          dragOver
            ? 'ring-1 ring-sky-400/70 bg-sky-500/10 text-zinc-100'
            : isActive
              ? 'bg-zinc-800 text-zinc-100'
              : 'text-zinc-400 hover:bg-zinc-900 hover:text-zinc-200',
        )}
        style={{ paddingLeft: 6 + depth * 12 }}
      >
        <Button variant="subtle" size="sm"
          type="button"
          onClick={() => setExpanded((v) => !v)}
          disabled={!hasChildren}
          className={cn(
            'flex h-4 w-4 shrink-0 items-center justify-center rounded text-zinc-500',
            hasChildren ? 'hover:text-zinc-200' : 'opacity-0',
          )}
          aria-label={expanded ? tr('折叠', 'Collapse') : tr('展开', 'Expand')}
        >
          <ChevronRight
            size={11}
            className={cn('transition-transform', expanded && 'rotate-90')}
          />
        </Button>
        <Hint content={localizedPath(node.path)}><Button variant="subtle" size="sm"
          type="button"
          onClick={() => onPick(node.path)}
          className="flex flex-1 items-center justify-between gap-1.5 truncate"

        >
          <span className="flex items-center gap-1.5 truncate">
            <Icon size={12} className="shrink-0" />
            <span className="truncate">{localizedPathSegment(node.name)}</span>
          </span>
          <span className="text-[10px] text-zinc-500">{node.subtreeCount}</span>
        </Button></Hint>
      </div>
      {expanded && hasChildren && (
        <div>
          {[...node.children.values()]
            .sort((a, b) => a.name.localeCompare(b.name))
            .map((c) => (
              <FolderNode
                key={c.path}
                node={c}
                depth={depth + 1}
                activePath={activePath}
                onPick={onPick}
                onMoveDoc={onMoveDoc}
              />
            ))}
        </div>
      )}
    </div>
  );
}

function DocCard({
  doc,
  onEdit,
  onDelete,
}: {
  doc: KnowledgeDoc;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const { tr } = useI18n();
  // Editable = the org's own content (manual paste / uploaded file).
  // Read-only = platform vault + (legacy) repo docs — regenerated on sync.
  const editable = doc.source_type === 'manual' || doc.source_type === 'upload';
  const sourceBadge =
    doc.source_type === 'vault'
      ? { label: tr('内置', 'built-in'), cls: 'text-amber-300' }
      : doc.source_type === 'upload'
        ? { label: tr('上传', 'upload'), cls: 'text-sky-300' }
        : doc.source_type === 'repo'
          ? { label: 'repo', cls: 'text-emerald-300' }
          : { label: tr('手动', 'manual'), cls: 'text-indigo-300' };
  return (
    <Card
      compact
      // Org docs (manual/upload) are draggable onto a folder in the 组织 tree
      // to relocate them (ADR-029). The drop targets read DOC_DND_MIME.
      draggable={editable}
      onDragStart={
        editable
          ? (e) => {
              e.dataTransfer.setData(DOC_DND_MIME, doc.id);
              e.dataTransfer.effectAllowed = 'move';
            }
          : undefined
      }
      className="flex cursor-pointer flex-col py-2.5 transition-colors hover:bg-zinc-800/40"
      onClick={onEdit}
    >
      <div className="flex items-center justify-between gap-3">
        <div className="min-w-0">
          <Hint content={localizedDocTitle(doc)}><div className="truncate text-sm font-medium text-zinc-100" >
            {localizedDocTitle(doc)}
          </div></Hint>
          <div className="mt-0.5 flex flex-wrap items-center gap-1.5 text-[10px] uppercase tracking-wider text-zinc-500">
            <span className={sourceBadge.cls}>{sourceBadge.label}</span>
            {doc.path && (
              <span className="rounded bg-zinc-800/60 px-1.5 py-0.5 font-normal normal-case text-zinc-300">
                {localizedPath(doc.path)}
              </span>
            )}
          </div>
          {doc.tags && doc.tags.length > 0 && (
            <div className="mt-1.5 flex flex-wrap gap-1">
              {doc.tags.map((t) => (
                <span
                  key={t}
                  className="rounded bg-zinc-800/60 px-1.5 py-0.5 text-[10px] text-zinc-400"
                >
                  {t}
                </span>
              ))}
            </div>
          )}
          {doc.url && (
            <Hint content={doc.url}><div className="mt-1 truncate font-mono text-[10px] text-zinc-600" >
              {doc.url}
            </div></Hint>
          )}
        </div>
        <div className="flex shrink-0 items-center gap-1">
          {editable ? (
            <>
              <Hint content={tr('编辑', 'Edit')}><Button variant="subtle" size="sm"
                type="button"
                onClick={(e) => {
                  e.stopPropagation();
                  onEdit();
                }}

                className="p-1"
              >
                <Pencil size={11} />
              </Button></Hint>
              <Hint content={tr('删除', 'Delete')}><Button variant="dangerGhost" size="sm"
                type="button"
                onClick={(e) => {
                  e.stopPropagation();
                  onDelete();
                }}

                className="p-1"
              >
                <Trash2 size={11} />
              </Button></Hint>
            </>
          ) : (
            <Hint content={tr('查看', 'View')}><Button variant="subtle" size="sm"
              type="button"
              onClick={(e) => {
                e.stopPropagation();
                onEdit();
              }}

              className="p-1"
            >
              <Eye size={11} />
            </Button></Hint>
          )}
        </div>
      </div>
    </Card>
  );
}

function SearchHitCard({ hit }: { hit: SearchHit }) {
  const { tr } = useI18n();
  return (
    <div className="rounded-md border border-zinc-800/60 bg-zinc-950/40 px-3 py-2">
      <div className="flex items-center justify-between text-[11px] text-zinc-500">
        <div className="flex items-center gap-2">
          <span className="text-zinc-100 font-medium">{localizedDocTitle(hit.doc)}</span>
          {hit.doc.path && (
            <span className="rounded bg-zinc-800/60 px-1.5 py-0.5 text-[10px] text-zinc-400">
              {localizedPath(hit.doc.path)}
            </span>
          )}
          {hit.doc.source_type === 'repo' && (
            <span className="rounded bg-emerald-500/10 px-1.5 py-0.5 text-[10px] text-emerald-300">
              repo
            </span>
          )}
          {hit.layer === 'wiki' && (
            <span className="rounded bg-indigo-500/10 px-1.5 py-0.5 text-[10px] text-indigo-300">
              LLM Wiki
            </span>
          )}
          {hit.matched_node && (
            <Chip tone="info" dense>
              {tr('命中知识节点', 'Matched node')}: {hit.matched_node}
            </Chip>
          )}
          {hit.layer === 'raw' && (
            <span className="rounded bg-sky-500/10 px-1.5 py-0.5 text-[10px] text-sky-300">
              {tr('RAG', 'RAG')}
            </span>
          )}
        </div>
        <span>score {hit.score.toFixed(2)}</span>
      </div>
      {/* 预览只给正文：frontmatter 占满 3 行预览毫无信息量 */}
      <div className="mt-1 line-clamp-3 whitespace-pre-wrap break-words text-[12px] text-zinc-300">
        {stripFrontmatter(hit.doc.content ?? '')}
      </div>
    </div>
  );
}

// 阅读态正文：先剥离 YAML frontmatter 再渲染 —— 否则 `title: …\n---`
// 会按 setext 标题渲染成粗体大字。元信息以弱化的 key/value 行单独展示。
function DocBody({ content, omitMetaKeys }: { content: string; omitMetaKeys?: readonly string[] }) {
  const fm = useMemo(() => splitFrontmatter(content), [content]);
  return (
    <div className="md-body text-sm text-zinc-200">
      {fm && (
        <dl className="mb-4 space-y-1 border-b border-zinc-800 pb-3">
          {fm.meta.filter(([key]) => !omitMetaKeys?.includes(key)).map(([key, value]) => (
            <div key={key} className="flex items-baseline gap-2 text-xs">
              <dt className="shrink-0 font-mono text-[11px] text-zinc-500">{key}</dt>
              {Array.isArray(value) ? (
                <dd className="flex flex-wrap gap-1">
                  {value.map((v) => (
                    <span key={v} className="rounded bg-zinc-800/60 px-1.5 py-0.5 text-[11px] text-zinc-400">
                      {v}
                    </span>
                  ))}
                </dd>
              ) : (
                <dd className="text-zinc-400">{value}</dd>
              )}
            </div>
          ))}
        </dl>
      )}
      {/* remark-gfm：表格 / 删除线 / 任务列表等 GFM 语法，
          缺了它表格按普通段落渲染成一行竖线文本 */}
      <ReactMarkdown remarkPlugins={[remarkGfm]}>{fm ? fm.body : content}</ReactMarkdown>
    </div>
  );
}

function DocEditor({
  mode,
  existing,
  defaultPath,
  onClose,
  onSaved,
}: {
  mode: 'create' | 'edit';
  existing: KnowledgeDoc | null;
  defaultPath?: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { tr } = useI18n();
  const [title, setTitle] = useState(existing?.title ?? '');
  const [titleEN, setTitleEN] = useState(existing?.title_en ?? '');
  const [content, setContent] = useState(existing?.content ?? '');
  const [url, setUrl] = useState(existing?.url ?? '');
  const [path, setPath] = useState(existing?.path ?? defaultPath ?? '');
  const [tagsText, setTagsText] = useState((existing?.tags ?? []).join(', '));
  const [submitting, setSubmitting] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  // Vault (built-in) and repo docs are read-only — both are regenerated
  // on sync, so in-place edits would be silently lost (the backend PATCH
  // rejects them too). Users can still open them to read the rendered
  // body; the modal switches to a markdown viewer instead of the form.
  // "复制为组织文档" forks the body into an editable org doc (manual),
  // which is the supported way to customize built-in content.
  const sourceReadOnly =
    existing?.source_type === 'repo' || existing?.source_type === 'vault';
  const [forked, setForked] = useState(false);
  const readOnly = mode === 'edit' && sourceReadOnly && !forked;
  // An uploaded file's url is its identity (the filename); it can't be
  // re-pointed from the editor — the backend keeps it fixed. Show it but
  // lock the field so editing it isn't silently ignored.
  const urlLocked = mode === 'edit' && existing?.source_type === 'upload';

  useEffect(() => {
    if (mode === 'edit' && existing && !content) {
      setLoading(true);
      void (async () => {
        try {
          const r = await import('@/api/knowledge').then((m) => m.getDoc(existing.id));
          setContent(r.content ?? '');
        } catch (e) {
          setErr(e instanceof ApiError ? e.message : (e as Error).message);
        } finally {
          setLoading(false);
        }
      })();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mode, existing?.id]);

  const submit = async () => {
    setSubmitting(true);
    setErr(null);
    try {
      const tags = tagsText
        .split(/[,，]/)
        .map((t) => t.trim())
        .filter(Boolean);
      const cleanPath = path
        .split('/')
        .map((s) => s.trim())
        .filter(Boolean)
        .join('/');
      if (mode === 'create' || forked) {
        // forked: a read-only vault/repo doc copied into the org scope —
        // lands as a brand-new manual doc, the original stays untouched.
        await createDoc({
          title: title.trim(),
          title_en: titleEN.trim() || undefined,
          content: content.trim(),
          url: forked ? undefined : url.trim() || undefined,
          path: cleanPath || undefined,
          tags: tags.length ? tags : undefined,
        });
      } else if (existing) {
        await updateDoc(existing.id, {
          title: title.trim(),
          title_en: titleEN.trim() || undefined,
          content: content.trim(),
          path: cleanPath || undefined,
          tags: tags.length ? tags : undefined,
        });
      }
      onSaved();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setSubmitting(false);
    }
  };

  // Read-only viewer for vault/repo docs — render markdown body, show
  // metadata in a header strip. Manual/upload docs continue to the editor
  // form below.
  if (readOnly) {
    const isVault = existing?.source_type === 'vault';
    return (
      <Modal
        open
        onClose={onClose}
        // 阅读态默认给到 max-w-4xl，并允许拖左右边缘自行调宽 ——
        // 内置 runbook 普遍带宽表格，md(448px) 下表格没法看。
        size="xl"
        resizable
        title={existing ? localizedDocTitle(existing) : tr('文档', 'Document')}
        footer={
          <>
            <Button variant="outline" size="sm"
              type="button"
              onClick={onClose}
              className="px-3 py-1.5"
            >
              {tr('关闭', 'Close')}
            </Button>
            <Hint content={
                isVault
                  ? tr('内置文档随同步更新、不可直接修改；复制一份到组织知识库后即可编辑', 'Built-in docs are refreshed on sync and not directly editable; copy into the org knowledge base to edit')
                  : tr('repo 文档随同步更新、不可直接修改；复制一份到组织知识库后即可编辑', 'Repo docs are refreshed on sync and not directly editable; copy into the org knowledge base to edit')
              }><Button variant="primary" size="sm"
              type="button"
              onClick={() => {
                // The copy is a fresh org doc — drop the source url
                // (builtin://… / repo path) so it isn't carried over.
                setUrl('');
                setForked(true);
              }}
              disabled={loading || !content}

              className="flex items-center gap-1.5 px-3 py-1.5 font-medium text-accent-fg"
            >
              <Copy size={11} />
              {tr('复制为组织文档', 'Copy as org doc')}
            </Button></Hint>
          </>
        }
      >
        <div className="space-y-3">
          <div className="flex flex-wrap items-center gap-2 text-[11px] text-zinc-500">
            {isVault ? (
              <span className="rounded bg-amber-500/15 px-1.5 py-0.5 text-amber-300">{tr('内置', 'built-in')}</span>
            ) : (
              <span className="rounded bg-emerald-500/15 px-1.5 py-0.5 text-emerald-300">repo</span>
            )}
            {existing?.path && (
              <span className="rounded bg-zinc-800/60 px-1.5 py-0.5 text-zinc-300">{localizedPath(existing.path)}</span>
            )}
            {existing?.tags?.map((t) => (
              <span key={t} className="rounded bg-zinc-800/60 px-1.5 py-0.5 text-zinc-400">
                {t}
              </span>
            ))}
            {existing?.url &&
              (existing.url.startsWith('http') ? (
                <Hint content={existing.url}><a
                  href={existing.url}
                  target="_blank"
                  rel="noreferrer"
                  className="ml-auto truncate font-mono text-[10px] text-zinc-500 hover:text-zinc-300 underline-offset-2 hover:underline"

                >
                  {existing.url}
                </a></Hint>
              ) : (
                <Hint content={existing.url}><span className="ml-auto truncate font-mono text-[10px] text-zinc-500" >
                  {existing.url}
                </span></Hint>
              ))}
          </div>
          {/* 不再自限 60vh：Modal 本体已 max-h-90vh + 内部滚动，
              嵌套两层滚动条阅读体验差 */}
          <div className="rounded-md border border-zinc-800 bg-zinc-950/40 p-4">
            {loading ? (
              <div className="text-xs text-zinc-500">{tr('加载中…', 'Loading…')}</div>
            ) : err ? (
              <div className="text-xs text-red-300">{err}</div>
            ) : content ? (
              <DocBody content={content} />
            ) : (
              <div className="text-xs text-zinc-500">{tr('该文档没有正文内容', 'This document has no body content')}</div>
            )}
          </div>
        </div>
      </Modal>
    );
  }

  return (
    <Modal
      open
      onClose={onClose}
      title={
        mode === 'create'
          ? tr('新建知识文档', 'New knowledge doc')
          : forked
            ? tr(`复制「${existing ? localizedDocTitle(existing) : ''}」为组织文档`, `Copy "${existing ? localizedDocTitle(existing) : ''}" as org doc`)
            : tr(`编辑 ${existing ? localizedDocTitle(existing) : ''}`, `Edit ${existing ? localizedDocTitle(existing) : ''}`)
      }
      footer={
        <>
          <Button variant="outline" size="sm"
            type="button"
            onClick={onClose}
            className="px-3 py-1.5"
          >
            {tr('取消', 'Cancel')}
          </Button>
          <Button variant="primary" size="sm"
            type="button"
            onClick={() => void submit()}
            disabled={submitting || title.trim() === '' || content.trim() === ''}
            className="px-3 py-1.5 font-medium text-accent-fg"
          >
            {submitting ? tr('保存中…', 'Saving…') : tr('保存', 'Save')}
          </Button>
        </>
      }
    >
      <div className="space-y-3 text-xs text-zinc-300">
        {err && (
          <div className="rounded-md border border-red-500/40 bg-red-500/5 px-3 py-2 text-red-300">{err}</div>
        )}
        <Label className="block">
          <div className="mb-1 text-[11px] text-zinc-500">{tr('标题 *', 'Title *')}</div>
          <Input
            type="text"
            value={title}
            onChange={(e) => setTitle(e.target.value)}
            placeholder={tr('例：nginx 重启 SOP', 'e.g. nginx restart SOP')}
            maxLength={256}
            className="w-full"
          />
        </Label>
        <Label className="block">
          <div className="mb-1 text-[11px] text-zinc-500">
            {tr('英文标题（可选；en-US locale 下显示）', 'English title (optional; shown in en-US locale)')}
          </div>
          <Input
            type="text"
            value={titleEN}
            onChange={(e) => setTitleEN(e.target.value)}
            placeholder={tr('例：nginx restart SOP', 'e.g. nginx restart SOP')}
            maxLength={256}
            className="w-full"
          />
        </Label>
        <div className="grid grid-cols-2 gap-2">
          <Label className="block">
            <div className="mb-1 text-[11px] text-zinc-500">{tr('目录路径（用 / 分隔）', 'Folder path (separated by /)')}</div>
            <Input
              type="text"
              value={path}
              onChange={(e) => setPath(e.target.value)}
              placeholder={tr('网络/DNS', 'Network/DNS')}
              className="w-full"
            />
          </Label>
          <Label className="block">
            <div className="mb-1 text-[11px] text-zinc-500">{tr('标签（逗号分隔）', 'Tags (comma separated)')}</div>
            <Input
              type="text"
              value={tagsText}
              onChange={(e) => setTagsText(e.target.value)}
              placeholder="dns, resolv"
              className="w-full"
            />
          </Label>
        </div>
        <Label className="block">
          <div className="mb-1 text-[11px] text-zinc-500">
            {urlLocked ? tr('来源文件（不可修改）', 'Source file (read-only)') : tr('来源 URL（可选）', 'Source URL (optional)')}
          </div>
          <Input
            type="text"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            readOnly={urlLocked}
            placeholder="https://wiki.internal/runbook/nginx-restart"
            className={cn(
              "w-full",
              urlLocked && "cursor-not-allowed",
            )}
          />
        </Label>
        <Label className="block">
          <div className="mb-1 text-[11px] text-zinc-500">{tr('内容（markdown）*', 'Content (markdown) *')}</div>
          <Textarea
            value={content}
            onChange={(e) => setContent(e.target.value)}
            placeholder={tr('支持 markdown。LLM 通过 query_knowledge 检索时按语义匹配 title + content。', 'Markdown supported. The LLM does semantic match over title + content via the query_knowledge tool.')}
            className="h-72 w-full resize-y font-mono"
          />
        </Label>
      </div>
    </Modal>
  );
}

function DeleteDocDialog({
  doc,
  onClose,
  onDone,
}: {
  doc: KnowledgeDoc;
  onClose: () => void;
  onDone: () => void;
}) {
  const { tr } = useI18n();
  const [submitting, setSubmitting] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const submit = async () => {
    setSubmitting(true);
    setErr(null);
    try {
      await deleteDoc(doc.id);
      onDone();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setSubmitting(false);
    }
  };
  return (
    <Modal
      open
      onClose={onClose}
      title={tr(`删除 ${localizedDocTitle(doc)}`, `Delete ${localizedDocTitle(doc)}`)}
      footer={
        <>
          <Button variant="outline" size="sm"
            type="button"
            onClick={onClose}
            className="px-3 py-1.5"
          >
            {tr('取消', 'Cancel')}
          </Button>
          <Button variant="danger" size="sm"
            type="button"
            onClick={() => void submit()}
            disabled={submitting}
            className="px-3 py-1.5 font-medium"
          >
            {submitting ? tr('删除中…', 'Deleting…') : tr('删除', 'Delete')}
          </Button>
        </>
      }
    >
      <div className="text-xs text-zinc-300">
        {err && (
          <div className="mb-3 rounded-md border border-red-500/40 bg-red-500/5 px-3 py-2 text-red-300">
            {err}
          </div>
        )}
        <p>{tr('确定删除这条文档？', 'Delete this document?')}</p>
      </div>
    </Modal>
  );
}

function DeleteLLMWikiFileDialog({
  file,
  onClose,
  onDone,
}: {
  file: LLMWikiNode;
  onClose: () => void;
  onDone: () => void;
}) {
  const { tr } = useI18n();
  const [submitting, setSubmitting] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const submit = async () => {
    setSubmitting(true);
    setErr(null);
    try {
      await deleteLLMWikiNode(file.id);
      onDone();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e as Error).message);
    } finally {
      setSubmitting(false);
    }
  };
  return (
    <Modal
      open
      onClose={onClose}
      title={tr(`删除 ${file.name}`, `Delete ${file.name}`)}
      footer={
        <>
          <button type="button" onClick={onClose} className="rounded-md border border-zinc-700 bg-zinc-900 px-3 py-1.5 text-xs text-zinc-300 hover:bg-zinc-800">
            {tr('取消', 'Cancel')}
          </button>
          <button type="button" onClick={() => void submit()} disabled={submitting} className="rounded-md bg-red-500 px-3 py-1.5 text-xs font-medium text-white hover:bg-red-600 disabled:opacity-50">
            {submitting ? tr('删除中…', 'Deleting…') : tr('确认删除', 'Delete')}
          </button>
        </>
      }
    >
      <div className="text-xs text-zinc-300">
        {err && <div className="mb-3 rounded-md border border-red-500/40 bg-red-500/5 px-3 py-2 text-red-300">{err}</div>}
        <p>{file.layer === 'raw'
          ? tr('删除 Raw 文件后，关联的 LLM Wiki 页面、来源关系和索引也会同步删除。此操作不可恢复。', 'Deleting this raw file also removes its related LLM Wiki pages, source relations, and search index. This cannot be undone.')
          : tr('删除该 Wiki 文件及其关联关系和索引，但不会删除对应的 Raw 文件。此操作不可恢复。', 'Deleting this Wiki file also removes its relations and search index, but keeps the corresponding Raw file. This cannot be undone.')}</p>
        <p className="mt-2 font-mono text-[11px] text-zinc-500">{file.relative_path}</p>
      </div>
    </Modal>
  );
}
