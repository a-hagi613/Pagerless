import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";

import {
  THEME_STORAGE_KEY,
  ThemeContext,
  applyTheme,
  initialTheme,
  prefersDark,
  resolveTheme,
  type ResolvedTheme,
  type Theme,
} from "./theme-context";

// Persists Light, Dark, or System and follows the operating system while System is selected. The
// resolved palette was already applied by the pre-paint script; this keeps it in sync afterwards.
export function ThemeProvider({ children }: { children: ReactNode }) {
  const [theme, setThemeState] = useState<Theme>(() => initialTheme());
  const [darkPreferred, setDarkPreferred] = useState<boolean>(() => prefersDark());

  useEffect(() => {
    const query = window.matchMedia("(prefers-color-scheme: dark)");
    const listener = (event: MediaQueryListEvent) => {
      setDarkPreferred(event.matches);
    };
    query.addEventListener("change", listener);
    return () => {
      query.removeEventListener("change", listener);
    };
  }, []);

  const resolved: ResolvedTheme = resolveTheme(theme, darkPreferred);

  useEffect(() => {
    applyTheme(resolved);
  }, [resolved]);

  const setTheme = useCallback((next: Theme) => {
    setThemeState(next);
    try {
      window.localStorage.setItem(THEME_STORAGE_KEY, next);
    } catch {
      // A blocked storage still gets the palette for this session.
    }
  }, []);

  const value = useMemo(() => ({ theme, resolved, setTheme }), [theme, resolved, setTheme]);

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}
