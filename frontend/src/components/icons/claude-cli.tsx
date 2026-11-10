import { cn } from '@/lib/utils';

interface ClaudeCliProps extends React.SVGProps<SVGSVGElement> {
    className?: string;
}

// A terminal-prompt mark for the CLI-backed provider, not a vendor logo.
function ClaudeCli({ className, ...props }: ClaudeCliProps) {
    return (
        <svg
            className={cn(className)}
            fill="none"
            stroke="currentColor"
            strokeLinecap="round"
            strokeLinejoin="round"
            strokeWidth="2"
            viewBox="0 0 24 24"
            {...props}
        >
            <title>Claude Code CLI</title>
            <rect
                height="16"
                rx="3"
                width="20"
                x="2"
                y="4"
            />
            <path d="m7 10 3 2-3 2" />
            <path d="M13 15h4" />
        </svg>
    );
}

export default ClaudeCli;
