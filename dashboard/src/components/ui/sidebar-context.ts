import { createContext, useContext } from "react";

export interface SidebarState {
  open: boolean;
  toggle: () => void;
  setOpen: (open: boolean) => void;
}

export const SidebarContext = createContext<SidebarState | null>(null);

export function useSidebar(): SidebarState {
  const context = useContext(SidebarContext);
  if (context === null) {
    throw new Error("useSidebar requires a SidebarProvider");
  }
  return context;
}
