import { providerIcons } from '@/components/icons/provider-icon';
import { ProviderType } from '@/graphql/types';
import { cn } from '@/lib/utils';

export interface ProviderStatusItem {
    configured: boolean;
    label: string;
    type: ProviderType;
}

interface ProvidersStatusPanelProps {
    items: ProviderStatusItem[];
}

// The server reports whether each provider type is configured in its environment,
// not whether its endpoint is reachable right now, so the panel says "Configured".
export function ProvidersStatusPanel({ items }: ProvidersStatusPanelProps) {
    const configuredItems = items.filter((item) => item.configured);
    const unconfiguredItems = items.filter((item) => !item.configured);
    const configuredCount = configuredItems.length;
    const claudeCli = items.find((item) => item.type === ProviderType.ClaudeCli);

    return (
        <section
            aria-labelledby="providers-status-title"
            className="bg-card/40 rounded-lg border p-4"
        >
            <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
                <h2
                    className="text-sm font-semibold tracking-wide"
                    id="providers-status-title"
                >
                    Model providers
                </h2>
                <p className="text-muted-foreground text-xs">
                    {configuredCount} of {items.length} configured on the server
                </p>
            </div>

            {configuredItems.length > 0 ? (
                <ul
                    aria-label="Configured providers"
                    className="mt-3 grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3 2xl:grid-cols-4"
                >
                    {configuredItems.map(({ label, type }) => {
                        const icon = providerIcons[type];
                        const Icon = icon?.icon;

                        return (
                            <li
                                className="bg-background flex min-w-0 items-center gap-2 rounded-md border px-3 py-2 text-sm"
                                key={type}
                            >
                                {Icon && (
                                    <Icon
                                        aria-hidden
                                        className={cn('size-4 shrink-0', icon.className)}
                                    />
                                )}
                                <span className="flex-1 truncate">{label}</span>
                                <span className="inline-flex shrink-0 items-center gap-1.5 text-xs text-emerald-500">
                                    <span
                                        aria-hidden
                                        className="size-1.5 rounded-full bg-emerald-500"
                                    />
                                    Configured
                                </span>
                            </li>
                        );
                    })}
                </ul>
            ) : (
                <p className="text-muted-foreground mt-3 text-sm">No provider is configured on the server yet.</p>
            )}

            {unconfiguredItems.length > 0 && (
                <div className="mt-3 flex flex-col gap-2 sm:flex-row sm:items-start">
                    <p
                        className="text-muted-foreground shrink-0 pt-1 text-xs"
                        id="providers-not-configured-title"
                    >
                        Not configured
                    </p>
                    <ul
                        aria-labelledby="providers-not-configured-title"
                        className="flex flex-wrap gap-1.5"
                    >
                        {unconfiguredItems.map(({ label, type }) => {
                            const Icon = providerIcons[type]?.icon;

                            return (
                                <li
                                    className="bg-muted/20 text-muted-foreground inline-flex items-center gap-1.5 rounded-md border px-2 py-1 text-xs"
                                    key={type}
                                >
                                    {Icon && (
                                        <Icon
                                            aria-hidden
                                            className="size-3.5 shrink-0"
                                        />
                                    )}
                                    {label}
                                </li>
                            );
                        })}
                    </ul>
                </div>
            )}

            {claudeCli && (
                <div
                    className="mt-3 rounded-md border border-orange-400/30 bg-orange-400/5 px-3 py-2 text-xs leading-relaxed"
                    role="note"
                >
                    <span className="font-medium text-orange-400">Claude Code CLI · </span>
                    {claudeCli.configured ? (
                        <span className="text-muted-foreground">
                            Calls run through the Claude subscription logged in on the host, via cmd/claudeworker. If
                            the worker stops, the login expires or the usage limit is reached, the flow reports it in
                            its messages.
                        </span>
                    ) : (
                        <span className="text-muted-foreground">
                            Set <code className="text-foreground">CLAUDE_CLI_WORKER_URL</code> and{' '}
                            <code className="text-foreground">CLAUDE_CLI_WORKER_TOKEN</code>, start cmd/claudeworker on
                            the host, then restart PentAGI. It then appears in each flow&apos;s provider menu.
                        </span>
                    )}
                </div>
            )}
        </section>
    );
}
