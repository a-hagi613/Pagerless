import { createContext } from "react";

export type Theme = "light" | "dark" | "system";
export type ResolvedTheme = "light" | "dark";

export interface ThemeState {
  theme: Theme;
  resolved: ResolvedTheme;
  setTheme: (theme: Theme) => void;
}

export const ThemeContext = createContext<ThemeState | null>(null);

export const THEME_STORAGE_KEY = "pagerless.theme";

export function isTheme(value: string | null): value is Theme {
  return value === "light" || value === "dark" || value === "system";
}

// ?theme= pins the palette for one load (screenshots, docs, and the accessibility detector) without
// touching the stored preference.
export function initialTheme(search: string = window.location.search): Theme {
  const override = new URLSearchParams(search).get("theme");
  if (isTheme(override)) {
    return override;
  }
  try {
    const stored = window.localStorage.getItem(THEME_STORAGE_KEY);
    if (isTheme(stored)) {
      return stored;
    }
  } catch {
    // Storage can be blocked; System is the answer that always works.
  }
  return "system";
}

export function prefersDark(): boolean {
  return window.matchMedia("(prefers-color-scheme: dark)").matches;
}

export function resolveTheme(theme: Theme, darkPreferred: boolean): ResolvedTheme {
  if (theme === "system") {
    return darkPreferred ? "dark" : "light";
  }
  return theme;
}

// Applied before React renders as well, from the inline script in index.html.
export function applyTheme(resolved: ResolvedTheme): void {
  const root = document.documentElement;
  root.classList.remove("light", "dark");
  root.classList.add(resolved);
  root.style.colorScheme = resolved;
}
