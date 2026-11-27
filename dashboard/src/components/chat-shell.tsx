import { useRef, useState, type ReactNode } from "react";

import { AppHeader } from "@/components/app-header";
import { HistorySidebar } from "@/components/history-sidebar";
import { ThemeToggle } from "@/components/theme-toggle";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";

// One shell for a new investigation and an existing conversation. The history rail is always present on
// a wide viewport; on a narrow one the same component opens in a focus-trapped sheet.
export function ChatShell({ children }: { children: ReactNode }) {
  const [historyOpen, setHistoryOpen] = useState(false);
  const historyTrigger = useRef<HTMLButtonElement | null>(null);

  return (
    <div className="min-h-svh bg-background">
      <a
        href="#main"
        className="sr-only z-50 rounded-md bg-card px-3 py-2 text-sm font-medium text-foreground focus:not-sr-only focus:fixed focus:top-2 focus:left-2"
      >
        Skip to conversation
      </a>
      <AppHeader onOpenHistory={() => setHistoryOpen(true)} historyTrigger={historyTrigger} />
      <div className="mx-auto flex w-full max-w-[1400px] items-start">
        <aside className="sticky top-14 hidden h-[calc(100svh-3.5rem)] w-72 shrink-0 overflow-y-auto border-r border-border-subtle lg:block">
          <HistorySidebar />
        </aside>
        <main id="main" className="min-w-0 flex-1">
          {children}
        </main>
      </div>

      <Sheet open={historyOpen} onOpenChange={setHistoryOpen}>
        <SheetContent
          side="left"
          className="w-[85%] max-w-sm gap-0 p-0"
          onCloseAutoFocus={(event) => {
            event.preventDefault();
            historyTrigger.current?.focus();
          }}
        >
          <SheetHeader>
            <SheetTitle>History</SheetTitle>
            <SheetDescription>Investigations shared by everyone in this installation.</SheetDescription>
          </SheetHeader>
          <div className="min-h-0 flex-1 overflow-y-auto">
            <HistorySidebar heading={false} onNavigate={() => setHistoryOpen(false)} />
          </div>
          <SheetFooter className="border-t border-border-subtle px-3 py-3">
            <div className="flex items-center justify-between gap-2">
              <span className="text-xs font-medium text-muted-foreground">Colour theme</span>
              <ThemeToggle />
            </div>
          </SheetFooter>
        </SheetContent>
      </Sheet>
    </div>
  );
}
