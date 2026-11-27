import type { ComponentPropsWithoutRef, ReactNode } from "react";
import ReactMarkdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";

import { cn } from "@/lib/utils";

// Model output is untrusted. react-markdown builds React elements instead of
// setting HTML, raw HTML is skipped, and links are restricted to explicit
// schemes, so an answer can format itself without injecting markup.
function safeUrl(url: string): string {
  const value = url.trim();
  if (value.startsWith("#")) {
    return value;
  }
  try {
    const parsed = new URL(value);
    if (parsed.protocol === "http:" || parsed.protocol === "https:" || parsed.protocol === "mailto:") {
      return value;
    }
  } catch {
    // A relative URL is not a source we can resolve from an answer.
  }
  return "";
}

function heading(level: 3 | 4) {
  return function Heading({ children }: { children?: ReactNode }) {
    const Tag = level === 3 ? "h3" : "h4";
    return (
      <Tag
        className={cn(
          "mt-3 mb-1 font-semibold text-foreground first:mt-0",
          level === 3 ? "text-sm" : "text-[13px]",
        )}
      >
        {children}
      </Tag>
    );
  };
}

const components: Components = {
  h1: heading(3),
  h2: heading(3),
  h3: heading(4),
  h4: heading(4),
  h5: heading(4),
  h6: heading(4),
  p: ({ children }) => <p className="my-1.5 text-sm leading-relaxed first:mt-0 last:mb-0">{children}</p>,
  ul: ({ children }) => <ul className="my-1.5 list-disc space-y-1 pl-5 text-sm leading-relaxed">{children}</ul>,
  ol: ({ children }) => <ol className="my-1.5 list-decimal space-y-1 pl-5 text-sm leading-relaxed">{children}</ol>,
  li: ({ children }) => <li className="pl-0.5 [&>p]:my-0">{children}</li>,
  strong: ({ children }) => <strong className="font-semibold text-foreground">{children}</strong>,
  em: ({ children }) => <em className="italic">{children}</em>,
  del: ({ children }) => <del className="text-muted-foreground line-through">{children}</del>,
  a: ({ href, children }) => (
    <a
      href={href}
      target="_blank"
      rel="noreferrer noopener"
      className="font-medium text-primary underline underline-offset-2"
    >
      {children}
    </a>
  ),
  blockquote: ({ children }) => (
    <blockquote className="my-2 border-l-2 border-primary/40 pl-3 text-sm text-muted-foreground">
      {children}
    </blockquote>
  ),
  hr: () => <hr className="my-3 border-border-subtle" />,
  pre: ({ children }) => (
    <pre className="my-2 overflow-x-auto rounded-md border border-border-subtle bg-muted p-3 font-mono text-xs leading-relaxed [&>code]:border-0 [&>code]:bg-transparent [&>code]:p-0">
      {children}
    </pre>
  ),
  code: ({ className, children }: ComponentPropsWithoutRef<"code">) => (
    <code
      className={cn(
        "rounded border border-border-subtle bg-muted px-1 py-0.5 font-mono text-[0.85em]",
        className,
      )}
    >
      {children}
    </code>
  ),
  table: ({ children }) => (
    <div className="my-2 overflow-x-auto">
      <table className="w-full border-collapse text-left text-xs">{children}</table>
    </div>
  ),
  th: ({ children }) => <th className="border-b border-border-subtle px-2 py-1 font-semibold">{children}</th>,
  td: ({ children }) => <td className="border-b border-border-subtle px-2 py-1 align-top">{children}</td>,
};

export function Markdown({ children }: { children: string }) {
  return (
    <div data-message-markdown className="min-w-0 break-words text-foreground">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={components}
        skipHtml
        urlTransform={safeUrl}
      >
        {children}
      </ReactMarkdown>
    </div>
  );
}
