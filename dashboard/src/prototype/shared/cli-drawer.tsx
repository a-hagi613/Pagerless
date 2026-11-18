import { SquareTerminal } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

import { CLI_SECTIONS } from "../data";
import type { Page } from "../types";
import { CommandCard } from "./command-card";

// Every mutation in the portal has a kubectl page verb. This drawer is the
// running proof of the CLI/UI parity the v2 portal promises.
export function CliDrawer({
  open,
  onOpenChange,
  page,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  page: Page | null;
}) {
  const pageCommands =
    page === null
      ? []
      : [
          {
            command: `kubectl page get ${page.id} -n ${page.namespace} -o yaml`,
            note: "The Page, its trace, and the pinned proposal hash.",
          },
          {
            command: `kubectl page diff ${page.id}`,
            note: "Renders the same unified diff the portal shows.",
          },
          {
            command: `kubectl page approve ${page.id} --create-pr`,
            note: "Equivalent to the Create PR button; records the approver.",
          },
          {
            command: `kubectl page dismiss ${page.id} --reason "handled elsewhere"`,
            note: "Equivalent to Dismiss; the trace is kept for audit.",
          },
        ];

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="sm:max-w-[600px]">
        <SheetHeader>
          <SheetTitle className="flex items-center gap-2">
            <SquareTerminal className="size-4 text-primary" aria-hidden="true" />
            CLI equivalent
          </SheetTitle>
          <SheetDescription>
            Every button in the portal has a verb. Copy a command and rebuild the same decision in a terminal.
          </SheetDescription>
        </SheetHeader>

        <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">
          <Tabs defaultValue={page === null ? "pages" : "page"}>
            <TabsList className="w-full">
              {page !== null && <TabsTrigger value="page">This Page</TabsTrigger>}
              {CLI_SECTIONS.map((section) => (
                <TabsTrigger key={section.label} value={section.label.toLowerCase()}>
                  {section.label}
                </TabsTrigger>
              ))}
            </TabsList>

            {page !== null && (
              <TabsContent value="page" className="mt-3 flex flex-col gap-2">
                <div className="flex items-center gap-2">
                  <Badge variant="mint">{page.id}</Badge>
                  <span className="truncate font-mono text-[12px] text-muted-foreground">
                    {page.kind.toLowerCase()}/{page.namespace}/{page.workload}
                  </span>
                </div>
                {pageCommands.map((item) => (
                  <CommandCard key={item.command} command={item.command} note={item.note} />
                ))}
              </TabsContent>
            )}

            {CLI_SECTIONS.map((section) => (
              <TabsContent key={section.label} value={section.label.toLowerCase()} className="mt-3 flex flex-col gap-2">
                <p className="text-[12px] leading-relaxed text-muted-foreground">{section.parity}</p>
                {section.commands.map((item) => (
                  <CommandCard key={item.command} command={item.command} note={item.note} />
                ))}
              </TabsContent>
            ))}
          </Tabs>
        </div>

        <div className="border-t border-border-subtle px-5 py-3">
          <p className="text-[12px] leading-relaxed text-muted-foreground">
            The token from onboarding is stored in{" "}
            <span className="font-mono text-foreground">~/.config/pagerless/config</span> by the plugin, so both
            surfaces share one session and one audit trail.
          </p>
        </div>
      </SheetContent>
    </Sheet>
  );
}
