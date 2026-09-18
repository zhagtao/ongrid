import { useEffect, useMemo, useState } from 'react';
import { ChevronRight, Folder, FolderOpen } from 'lucide-react';
import { cn } from '@/lib/cn';
import { tr as trInline } from '@/i18n/locale';
import { localizedPathSegment } from '@/api/knowledge';
import { type LLMWikiNode } from './types';

type WikiLayer = LLMWikiNode['layer'];

type Props = {
  nodes: LLMWikiNode[];
  layers?: WikiLayer[];
  documentCounts: Partial<Record<WikiLayer, number>>;
  activeDirectory: LLMWikiNode | null;
  activeLayer: 'all' | WikiLayer;
  onSelectDirectory: (node: LLMWikiNode | null, layer: 'all' | WikiLayer) => void;
};

const layerOrder: WikiLayer[] = ['raw', 'wiki'];

export function LLMWikiTree({ nodes, layers = layerOrder, documentCounts, activeDirectory, activeLayer, onSelectDirectory }: Props) {
  const { byParent, roots } = useMemo(() => {
    const byParent = new Map<string, LLMWikiNode[]>();
    for (const node of nodes) {
      if (node.kind !== 'folder') continue;
      const children = byParent.get(node.parent_id) ?? [];
      children.push(node);
      byParent.set(node.parent_id, children);
    }
    for (const children of byParent.values()) {
      children.sort((left, right) => left.name.localeCompare(right.name));
    }
    const roots = layers.map((layer) => ({
      id: `${layer}:root`,
      parent_id: '',
      layer,
      kind: 'folder' as const,
      name: layerLabel(layer),
      relative_path: '',
      has_children: (nodes.some((node) => node.layer === layer && node.kind === 'folder')),
      child_count: byParent.get('')?.filter((node) => node.layer === layer).length ?? 0,
      document_count: documentCounts[layer] ?? nodes.filter((node) => node.layer === layer && node.kind === 'file').length,
    }));
    return { byParent, roots };
  }, [documentCounts, layers, nodes]);

  const [expanded, setExpanded] = useState<Set<string>>(() => new Set(roots.map((root) => root.id)));

  useEffect(() => {
    setExpanded((current) => {
      const next = new Set(current);
      for (const root of roots) next.add(root.id);
      return next;
    });
  }, [roots]);

  const toggle = (id: string) => {
    setExpanded((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  return (
    <div className="space-y-1">
      {roots.map((root) => (
        <WikiTreeBranch
          key={root.id}
          node={root}
          children={byParent.get('')?.filter((node) => node.layer === root.layer) ?? []}
          byParent={byParent}
          expanded={expanded}
          activeDirectory={activeDirectory}
          activeLayer={activeLayer}
          onToggle={toggle}
          onSelectDirectory={onSelectDirectory}
          onRootSelect={() => onSelectDirectory(null, root.layer)}
          depth={0}
        />
      ))}
    </div>
  );
}

function WikiTreeBranch({
  node,
  children,
  byParent,
  expanded,
  activeDirectory,
  activeLayer,
  onToggle,
  onSelectDirectory,
  onRootSelect,
  depth,
}: {
  node: LLMWikiNode;
  children: LLMWikiNode[];
  byParent: Map<string, LLMWikiNode[]>;
  expanded: Set<string>;
  activeDirectory: LLMWikiNode | null;
  activeLayer: 'all' | WikiLayer;
  onToggle: (id: string) => void;
  onSelectDirectory: (node: LLMWikiNode | null, layer: 'all' | WikiLayer) => void;
  onRootSelect: () => void;
  depth: number;
}) {
  const isOpen = expanded.has(node.id);
  const isRoot = node.relative_path === '';
  const isSelected = isRoot
    ? activeLayer === node.layer && !activeDirectory
    : activeDirectory?.id === node.id;
  return (
    <div>
      <div className="flex items-center gap-1" style={{ paddingLeft: `${depth * 12}px` }}>
        <button
          type="button"
          aria-label={isOpen ? 'Collapse' : 'Expand'}
          aria-expanded={isOpen}
          onClick={() => onToggle(node.id)}
          className="rounded p-1 text-zinc-600 hover:bg-zinc-900 hover:text-zinc-300"
        >
          <ChevronRight size={12} className={cn('transition-transform', isOpen && 'rotate-90')} />
        </button>
        <button
          type="button"
          onClick={isRoot ? onRootSelect : () => onSelectDirectory(node, node.layer)}
          className={cn('flex min-w-0 flex-1 items-center gap-2 rounded-md px-1.5 py-1.5 text-left text-xs', isSelected ? 'bg-zinc-800 text-zinc-100' : 'text-zinc-400 hover:bg-zinc-900 hover:text-zinc-200')}
        >
          {isOpen ? <FolderOpen size={14} className="shrink-0 text-zinc-500" /> : <Folder size={14} className="shrink-0 text-zinc-500" />}
          <span className="min-w-0 flex-1 truncate">{node.kind === 'folder' ? localizedPathSegment(node.name) : node.name}</span>
          {node.document_count > 0 && <span className="shrink-0 text-[10px] text-zinc-600">{node.document_count}</span>}
        </button>
      </div>
      {isOpen && children.map((child) => (
        <WikiTreeBranch
          key={child.id}
          node={child}
          children={byParent.get(child.id) ?? []}
          byParent={byParent}
          expanded={expanded}
          activeDirectory={activeDirectory}
          activeLayer={activeLayer}
          onToggle={onToggle}
          onSelectDirectory={onSelectDirectory}
          onRootSelect={onRootSelect}
          depth={depth + 1}
        />
      ))}
    </div>
  );
}

function layerLabel(layer: WikiLayer): string {
  if (layer === 'raw') return trInline('原始来源', 'Raw sources');
  return trInline('生成页面', 'Generated pages');
}
