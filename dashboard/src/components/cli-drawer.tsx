import { Link } from "react-router-dom";
import { SquareTerminal } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { CommandBlock } from "@/components/command-block";
import { CLI_GROUPS, CLI_VERBS, PORTAL_ACTIONS, thisPageCommands, verbsInGroup } from "@/lib/cli-parity";
import type { PageSummary } from "@/lib/types";

// Every Portal verb has a command here and every command has a Portal path; the parity test holds the
// two lists to a bijection.
export function CliDrawer({
  open,
  onOpenChange,
  page,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  page: PageSummary | null;
}) {
  const groups = CLI_GROUPS;

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="sm:max-w-[640px]">
        <SheetHeader>
          <SheetTitle className="flex items-center gap-2">
            <SquareTerminal className="size-4 text-primary" aria-hidden="true" />
            CLI equivalent
          </SheetTitle>
          <SheetDescription>
            Every control in the Portal has a <span className="font-mono">kubectl page</span> verb. Copy the
            command to repeat the same operation in a terminal; both surfaces write the same Page.
          </SheetDescription>
        </SheetHeader>

        <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">
          <div className="mb-3 flex flex-wrap items-center gap-2">
            <Badge variant="mint">
              {PORTAL_ACTIONS.length} Portal actions
            </Badge>
            <Badge variant="muted">{CLI_VERBS.length} CLI verbs</Badge>
            <span className="text-xs text-muted-foreground">paired one to one</span>
          </div>

          <Tabs defaultValue={page === null ? (groups[0] ?? "Pages") : "page"}>
            <TabsList className="w-full">
              {page !== null && <TabsTrigger value="page">This Page</TabsTrigger>}
              {groups.map((group) => (
                <TabsTrigger key={group} value={group}>
                  {group}
                </TabsTrigger>
              ))}
            </TabsList>

            {page !== null && (
              <TabsContent value="page" className="mt-3 flex flex-col gap-2">
                <p className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                  <Badge variant="outline" className="font-mono">
                    {page.name}
                  </Badge>
                  <span className="truncate font-mono">
                    {page.target.kind.toLowerCase()}/
                    {page.target.namespace === "" ? "" : `${page.target.namespace}/`}
                    {page.target.name}
                  </span>
                </p>
                {thisPageCommands({ name: page.name }).map((item) => (
                  <CommandBlock key={item.command} command={item.command} note={item.note} />
                ))}
              </TabsContent>
            )}

            {groups.map((group) => (
              <TabsContent key={group} value={group} className="mt-3 flex flex-col gap-2">
                {verbsInGroup(group).map((verb) => (
                  <CommandBlock key={verb.id} command={verb.invocation} note={verb.description} />
                ))}
              </TabsContent>
            ))}
          </Tabs>
        </div>

        <div className="border-t border-border-subtle px-5 py-3">
          <p className="text-xs leading-relaxed text-muted-foreground">
            The session you are holding is the same one{" "}
            <span className="font-mono text-foreground">kubectl page</span> uses in server mode; credentials and
            fingerprints are visible on the{" "}
            <Link
              to="/settings"
              className="text-mint underline-offset-2 hover:underline"
              onClick={() => {
                onOpenChange(false);
              }}
            >
              settings route
            </Link>
            .
          </p>
        </div>
      </SheetContent>
    </Sheet>
  );
}
