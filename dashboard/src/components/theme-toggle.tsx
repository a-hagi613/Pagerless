import { Monitor, Moon, Sun } from "lucide-react";

import { useTheme } from "@/lib/use-theme";
import type { Theme } from "@/lib/theme-context";
import { cn } from "@/lib/utils";

const options: { theme: Theme; label: string; Icon: typeof Sun }[] = [
  { theme: "light", label: "Light", Icon: Sun },
  { theme: "dark", label: "Dark", Icon: Moon },
  { theme: "system", label: "System", Icon: Monitor },
];

// Three peers, not a switch. Native radio inputs carry the keyboard model and the announced state; the
// visual segment is the label, and the focus ring rides on the input rather than on colour alone.
export function ThemeToggle({ className }: { className?: string }) {
  const { theme, resolved, setTheme } = useTheme();

  return (
    <div
      role="group"
      aria-label="Colour theme"
      className={cn("inline-flex items-center gap-0.5 rounded-lg border border-border bg-card p-0.5", className)}
    >
      {options.map(({ theme: value, label, Icon }) => {
        const checked = theme === value;
        return (
          <label
            key={value}
            title={value === "system" ? `System theme (currently ${resolved})` : `${label} theme`}
            className={cn(
              "relative inline-flex h-7 cursor-pointer items-center gap-1.5 rounded-md px-2 text-xs font-medium transition-colors",
              checked
                ? "bg-primary-soft text-primary-soft-foreground"
                : "text-muted-foreground hover:bg-accent hover:text-accent-foreground",
            )}
          >
            <input
              type="radio"
              name="pagerless-theme"
              value={value}
              checked={checked}
              onChange={() => {
                setTheme(value);
              }}
              aria-label={`${label} theme`}
              className="peer absolute inset-0 z-10 cursor-pointer opacity-0"
            />
            <Icon className="pointer-events-none size-3.5" aria-hidden="true" />
            <span className="pointer-events-none">{label}</span>
            <span
              aria-hidden="true"
              className="pointer-events-none absolute inset-0 rounded-md opacity-0 peer-focus-visible:opacity-100 peer-focus-visible:outline-2 peer-focus-visible:outline-offset-2 peer-focus-visible:outline-ring"
            />
          </label>
        );
      })}
    </div>
  );
}
