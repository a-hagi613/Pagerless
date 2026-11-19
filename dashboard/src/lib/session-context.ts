import { createContext } from "react";

import type { Session } from "./types";

export interface SessionState {
  // null means the server rejected the session: the Portal shows the sign-in route.
  session: Session | null;
  loading: boolean;
  error: string | null;
  refresh: () => Promise<void>;
}

export const SessionContext = createContext<SessionState | null>(null);
