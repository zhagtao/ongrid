import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { LLMWikiTree } from './LLMWikiTree';
import type { LLMWikiNode } from './types';

describe('LLMWikiTree', () => {
  it('localizes source directory names without changing file names', () => {
    const nodes: LLMWikiNode[] = [
      {
        id: 'raw-network',
        parent_id: '',
        layer: 'raw',
        kind: 'folder',
        name: 'network',
        relative_path: 'network',
        has_children: false,
        child_count: 0,
        document_count: 1,
      },
    ];

    render(
      <LLMWikiTree
        nodes={nodes}
        documentCounts={{ raw: 1, wiki: 0 }}
        activeDirectory={null}
        activeLayer="all"
        onSelectDirectory={vi.fn()}
      />,
    );

    expect(screen.getByText('网络')).toBeInTheDocument();
    expect(screen.queryByText('Network')).not.toBeInTheDocument();
  });
});
