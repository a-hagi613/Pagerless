import { useContext } from "react";

import { SessionContext, type SessionState } from "./session-context";

export function useSession(): SessionState {
  const state = useContext(SessionContext);
  if (state === null) {
    throw new Error("useSession must be used inside SessionProvider");
  }
  return state;
}
