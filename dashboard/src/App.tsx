import { Navigate, Route, Routes } from "react-router-dom";

import { BrandLockup } from "@/components/brand";
import { ChatShell } from "@/components/chat-shell";
import { InlineAlert } from "@/components/inline-alert";
import { ThemeToggle } from "@/components/theme-toggle";
import { Button } from "@/components/ui/button";
import { ChatPage } from "@/pages/chat";
import { SignInPage } from "@/pages/sign-in";
import { useAuth } from "@/lib/use-auth";

function BootScreen() {
  return (
    <div className="flex min-h-svh items-center justify-center bg-background px-4">
      <div className="flex flex-col items-center gap-3" role="status">
        <BrandLockup />
        <p className="text-sm text-muted-foreground">Connecting to Pagerless…</p>
      </div>
    </div>
  );
}

function Unreachable({ message, retry }: { message: string; retry: () => void }) {
  return (
    <div className="mx-auto flex w-full max-w-[560px] flex-col gap-3 px-4 py-16">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <BrandLockup />
        <ThemeToggle />
      </div>
      <h1 className="mt-2 text-xl font-semibold text-foreground">Pagerless is not reachable</h1>
      <InlineAlert
        title="The installation did not answer"
        action={
          <Button size="sm" variant="outline" onClick={retry}>
            Try again
          </Button>
        }
      >
        {message}
      </InlineAlert>
      <p className="text-sm text-muted-foreground">
        Check that the service is running and that the port-forward is still open. No investigation was
        started.
      </p>
    </div>
  );
}

function NotFound() {
  return (
    <ChatShell>
      <div className="mx-auto flex w-full max-w-3xl flex-col gap-3 px-4 py-16">
        <h1 className="text-xl font-semibold text-foreground">This page does not exist</h1>
        <p className="text-sm text-muted-foreground">
          Conversations live under <span className="font-mono text-foreground">/pages/&lt;id&gt;</span>. Start
          a new investigation to create one.
        </p>
        <Button asChild size="sm" variant="outline" className="w-fit">
          <a href="/">New investigation</a>
        </Button>
      </div>
    </ChatShell>
  );
}

export function App() {
  const auth = useAuth();

  if (auth.status === "checking") {
    return <BootScreen />;
  }

  if (auth.status === "unreachable") {
    return (
      <Unreachable
        message={auth.error ?? "The request failed."}
        retry={() => {
          void auth.retryProbe();
        }}
      />
    );
  }

  if (auth.status === "required") {
    return (
      <Routes>
        <Route path="/sign-in" element={<SignInPage />} />
        <Route path="*" element={<Navigate to="/sign-in" replace />} />
      </Routes>
    );
  }

  return (
    <Routes>
      <Route path="/" element={<ChatPage />} />
      <Route path="/pages/:pageId" element={<ChatPage />} />
      <Route path="/sign-in" element={<Navigate to="/" replace />} />
      <Route path="*" element={<NotFound />} />
    </Routes>
  );
}
