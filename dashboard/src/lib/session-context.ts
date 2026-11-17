import { createContext } from "react";

import type { Session } from "./api";

// SessionContext carries the signed-in user to the components that render it.
export const SessionContext = createContext<Session | null>(null);
