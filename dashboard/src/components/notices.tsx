import { AlertTriangle, Inbox } from "lucide-react";
import type { ReactNode } from "react";

import { Card, CardContent } from "./ui/card";

export function ErrorNotice({ children }: { children: ReactNode }) {
  return (
    <Card className="border-rose-200 bg-rose-50">
      <CardContent className="flex items-start gap-2 text-sm text-rose-800">
        <AlertTriangle aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
        <div role="alert">{children}</div>
      </CardContent>
    </Card>
  );
}

export function EmptyNotice({ children }: { children: ReactNode }) {
  return (
    <Card>
      <CardContent className="flex flex-col items-center gap-2 py-10 text-center text-sm text-slate-500">
        <Inbox aria-hidden="true" className="size-6 text-slate-400" />
        <div>{children}</div>
      </CardContent>
    </Card>
  );
}
