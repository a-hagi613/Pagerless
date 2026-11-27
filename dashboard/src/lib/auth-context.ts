import { createContext } from "react";

import type { PageSummary } from "./contract";

export type AuthStatus = "checking" | "authenticated" | "required" | "unreachable";

export type HistoryStatus = "loading" | "ready" | "error";

export interface AuthState {
  status: AuthStatus;
  // Set when the availability probe failed for a reason other than a refused session.
  error: string | null;
  pages: PageSummary[];
  historyStatus: HistoryStatus;
  historyError: string | null;
  exchange: (token: string) => Promise<void>;
  refreshHistory: (quiet?: boolean) => Promise<void>;
  retryProbe: () => Promise<void>;
}

export const AuthContext = createContext<AuthState | null>(null);
