import { useCallback, useMemo, useState, type ComponentProps, type ReactNode } from "react";
import { PanelLeftClose, PanelLeftOpen } from "lucide-react";

import { cn } from "@/lib/utils";

import { Button } from "./button";
import { SidebarContext, useSidebar } from "./sidebar-context";

// A compact take on the shadcn sidebar composition: the provider owns the
// collapsed state, and the primitives stay presentational so each variant can
// lay them out differently.
export function SidebarProvider({
  children,
  defaultOpen = true,
}: {
  children: ReactNode;
  defaultOpen?: boolean;
}) {
  const [open, setOpen] = useState(defaultOpen);
  const toggle = useCallback(() => {
    setOpen((current) => !current);
  }, []);
  const value = useMemo(() => ({ open, toggle, setOpen }), [open, toggle]);
  return (
    <SidebarContext.Provider value={value}>
      <div className="flex min-h-svh w-full" data-slot="sidebar-wrapper">
        {children}
      </div>
    </SidebarContext.Provider>
  );
}

export function Sidebar({ className, children, ...props }: ComponentProps<"aside">) {
  const { open } = useSidebar();
  return (
    <aside
      data-slot="sidebar"
      data-state={open ? "expanded" : "collapsed"}
      className={cn(
        "group/sidebar sticky top-0 hidden h-svh shrink-0 flex-col border-r border-border-subtle bg-surface-rail transition-[width] duration-200 lg:flex",
        open ? "w-60" : "w-[68px]",
        className,
      )}
      {...props}
    >
      {children}
    </aside>
  );
}

export function SidebarHeader({ className, ...props }: ComponentProps<"div">) {
  return (
    <div
      data-slot="sidebar-header"
      className={cn("flex items-center gap-2 px-3 py-3", className)}
      {...props}
    />
  );
}

export function SidebarContent({ className, ...props }: ComponentProps<"div">) {
  return (
    <div
      data-slot="sidebar-content"
      className={cn("flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-2 py-2", className)}
      {...props}
    />
  );
}

export function SidebarFooter({ className, ...props }: ComponentProps<"div">) {
  return (
    <div
      data-slot="sidebar-footer"
      className={cn("mt-auto flex flex-col gap-2 border-t border-border-subtle p-2", className)}
      {...props}
    />
  );
}

export function SidebarGroup({ className, ...props }: ComponentProps<"div">) {
  return <div data-slot="sidebar-group" className={cn("flex flex-col gap-1", className)} {...props} />;
}

export function SidebarGroupLabel({ className, ...props }: ComponentProps<"div">) {
  const { open } = useSidebar();
  return (
    <div
      data-slot="sidebar-group-label"
      className={cn(
        "px-2 py-1 font-mono text-[12px] tracking-[0.16em] text-muted-foreground uppercase",
        !open && "sr-only",
        className,
      )}
      {...props}
    />
  );
}

export function SidebarMenu({ className, ...props }: ComponentProps<"ul">) {
  return <ul data-slot="sidebar-menu" className={cn("flex flex-col gap-0.5", className)} {...props} />;
}

export function SidebarMenuItem({ className, ...props }: ComponentProps<"li">) {
  return <li data-slot="sidebar-menu-item" className={cn("relative", className)} {...props} />;
}

export function SidebarMenuButton({
  className,
  active = false,
  children,
  ...props
}: ComponentProps<"button"> & { active?: boolean }) {
  const { open } = useSidebar();
  return (
    <button
      data-slot="sidebar-menu-button"
      data-active={active ? "true" : undefined}
      className={cn(
        "flex h-9 w-full items-center gap-2.5 rounded-md px-2 text-left text-sm text-muted-foreground transition-colors hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring data-[active=true]:bg-primary/12 data-[active=true]:text-foreground",
        !open && "justify-center px-0",
        className,
      )}
      {...props}
    >
      {children}
    </button>
  );
}

export function SidebarTrigger({ className, ...props }: ComponentProps<"button">) {
  const { open, toggle } = useSidebar();
  return (
    <Button
      variant="ghost"
      size="icon-sm"
      aria-label={open ? "Collapse sidebar" : "Expand sidebar"}
      aria-expanded={open}
      data-slot="sidebar-trigger"
      className={cn("text-muted-foreground", className)}
      onClick={toggle}
      {...props}
    >
      {open ? <PanelLeftClose /> : <PanelLeftOpen />}
    </Button>
  );
}

export function SidebarInset({ className, ...props }: ComponentProps<"div">) {
  return (
    <div
      data-slot="sidebar-inset"
      className={cn("flex min-w-0 flex-1 flex-col bg-background", className)}
      {...props}
    />
  );
}
