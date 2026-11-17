import { useContext } from "react";

import type { Session } from "./api";
import { SessionContext } from "./session-context";

// useSession returns the signed-in user. It must be called below the provider.
export function useSession(): Session {
  const session = useContext(SessionContext);
  if (session === null) {
    throw new Error("useSession requires a SessionProvider");
  }
  return session;
}
