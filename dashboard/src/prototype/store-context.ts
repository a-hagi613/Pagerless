import { createContext, useContext } from "react";

import type { Page, ProtoSession, ResourceKind, Settings, SessionRole, Toast } from "./types";

export interface PrototypeApi {
  pages: Page[];
  clusterName: string;
  session: ProtoSession;
  settings: Settings;
  toasts: Toast[];
  activePageId: string | null;
  streamingPageIds: string[];
  selectPage: (id: string | null) => void;
  createPullRequest: (id: string) => void;
  dismissPage: (id: string) => void;
  pageResource: (target: { namespace: string; workload: string; kind: ResourceKind }) => string;
  signIn: (role: SessionRole) => void;
  updateSettings: (patch: Partial<Settings>) => void;
  notify: (title: string, detail: string, tone?: Toast["tone"]) => void;
  dismissToast: (id: number) => void;
}

export const PrototypeContext = createContext<PrototypeApi | null>(null);

export function usePrototype(): PrototypeApi {
  const value = useContext(PrototypeContext);
  if (value === null) {
    throw new Error("usePrototype requires the PrototypeProvider");
  }
  return value;
}
