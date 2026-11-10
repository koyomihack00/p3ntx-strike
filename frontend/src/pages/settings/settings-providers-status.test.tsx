import { render, screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { ProviderType } from '@/graphql/types';

import { ProvidersStatusPanel } from './settings-providers-status';

const items = (claudeCliConfigured: boolean) => [
    { configured: true, label: 'Anthropic', type: ProviderType.Anthropic },
    { configured: claudeCliConfigured, label: 'Claude Code CLI', type: ProviderType.ClaudeCli },
    { configured: false, label: 'OpenAI', type: ProviderType.Openai },
];

// The icons carry the same label in their SVG <title>, so SVG children are skipped.
const labelsIn = (list: HTMLElement) =>
    within(list)
        .getAllByRole('listitem')
        .map((item) =>
            Array.from(item.childNodes)
                .filter((node) => node.nodeName.toLowerCase() !== 'svg')
                .map((node) => node.textContent)
                .join('')
                .replace('Configured', '')
                .trim(),
        );

describe('ProvidersStatusPanel', () => {
    it('groups providers by whether they are configured, stating it in words', () => {
        render(<ProvidersStatusPanel items={items(false)} />);

        const configured = screen.getByRole('list', { name: 'Configured providers' });
        expect(labelsIn(configured)).toEqual(['Anthropic']);
        expect(within(configured).getByText('Configured')).toBeInTheDocument();

        expect(labelsIn(screen.getByRole('list', { name: 'Not configured' }))).toEqual(['Claude Code CLI', 'OpenAI']);
        expect(screen.getByText('1 of 3 configured on the server')).toBeInTheDocument();
    });

    it('tells how to enable Claude Code CLI while it is not configured', () => {
        render(<ProvidersStatusPanel items={items(false)} />);

        const note = screen.getByRole('note');
        expect(note).toHaveTextContent('CLAUDE_CLI_WORKER_URL');
        expect(note).toHaveTextContent('CLAUDE_CLI_WORKER_TOKEN');
        expect(note).toHaveTextContent('start cmd/claudeworker');
    });

    it('explains where Claude Code CLI failures surface once it is configured', () => {
        render(<ProvidersStatusPanel items={items(true)} />);

        const note = screen.getByRole('note');
        expect(note).toHaveTextContent('logged in on the host');
        expect(note).toHaveTextContent('the login expires or the usage limit is reached');
        expect(note).not.toHaveTextContent('CLAUDE_CLI_WORKER_URL');
        expect(labelsIn(screen.getByRole('list', { name: 'Configured providers' }))).toEqual([
            'Anthropic',
            'Claude Code CLI',
        ]);
    });

    it('says so when no provider is configured', () => {
        render(<ProvidersStatusPanel items={items(false).map((i) => ({ ...i, configured: false }))} />);

        expect(screen.queryByRole('list', { name: 'Configured providers' })).not.toBeInTheDocument();
        expect(screen.getByText('No provider is configured on the server yet.')).toBeInTheDocument();
        expect(screen.getByText('0 of 3 configured on the server')).toBeInTheDocument();
    });

    it('shows no Claude Code CLI note when that type is absent', () => {
        render(<ProvidersStatusPanel items={items(true).filter((i) => i.type !== ProviderType.ClaudeCli)} />);

        expect(screen.queryByRole('note')).not.toBeInTheDocument();
    });
});
