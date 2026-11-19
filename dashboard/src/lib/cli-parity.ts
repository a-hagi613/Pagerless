// True 50/50 CLI and Portal parity.
//
// Every capability the Portal offers has exactly one `kubectl page` verb, and every verb has exactly one
// Portal path. The drawer renders this manifest and the parity test asserts the bijection, so a
// capability added to one surface without the other fails the build rather than drifting. The verbs are
// the ones cmd/kubectl-page actually registers.

export type CliVerbId =
  | "new"
  | "list"
  | "get"
  | "diff"
  | "approve"
  | "dismiss"
  | "config-get"
  | "config-set"
  | "config-test"
  | "config-unset"
  | "login"
  | "version";

export type CliGroup = "Pages" | "Decisions" | "Setup";

export interface CliVerb {
  id: CliVerbId;
  group: CliGroup;
  invocation: string;
  description: string;
}

export const CLI_VERBS: readonly CliVerb[] = [
  {
    id: "new",
    group: "Pages",
    invocation: "kubectl page new deployment/checkout -n shop --hint <text>",
    description: "Opens a Page for a workload or a node.",
  },
  {
    id: "list",
    group: "Pages",
    invocation: "kubectl page list --phase Proposed",
    description: "Prints the same rows, order, and columns the inbox renders, narrowed by phase when asked.",
  },
  {
    id: "get",
    group: "Pages",
    invocation: "kubectl page get <name> --watch",
    description: "Prints the diagnosis, Findings, trace, and Proposal summary, and follows new steps.",
  },
  {
    id: "diff",
    group: "Decisions",
    invocation: "kubectl page diff <name>",
    description: "Renders the stored Proposal as the same unified diff.",
  },
  {
    id: "approve",
    group: "Decisions",
    invocation: "kubectl page approve <name>",
    description: "Records the approval for the displayed Proposal hash; the controller opens the pull request.",
  },
  {
    id: "dismiss",
    group: "Decisions",
    invocation: 'kubectl page dismiss <name> --reason "<text>"',
    description: "Rejects the Proposal, closes the Page without a pull request, and keeps it in the audit trail.",
  },
  {
    id: "config-get",
    group: "Setup",
    invocation: "kubectl page config get",
    description: "Prints the masked settings, fingerprints, and external ownership.",
  },
  {
    id: "config-set",
    group: "Setup",
    invocation: "kubectl page config set ai|github|connector|server",
    description: "Writes the AI endpoint, Target Repository, connector, or server session.",
  },
  {
    id: "config-test",
    group: "Setup",
    invocation: "kubectl page config test --server <url> --target ai|github|connector:<name>",
    description: "Runs the same connection test the settings buttons run.",
  },
  {
    id: "config-unset",
    group: "Setup",
    invocation: "kubectl page config unset AI_API_KEY",
    description: "Clears one credential key without touching the others.",
  },
  {
    id: "login",
    group: "Setup",
    invocation: "kubectl page login --server https://pagerless.example.com",
    description: "Stores the session that server-mode commands use, the session the Portal signs in with.",
  },
  {
    id: "version",
    group: "Setup",
    invocation: "kubectl page version -o json",
    description: "Prints the server, chart, and plugin versions the Portal reports.",
  },
];

export type PortalSurface = "header" | "matrix" | "inbox" | "card" | "settings" | "login" | "footer";

export interface PortalAction {
  id: string;
  label: string;
  surface: PortalSurface;
  cliVerb: CliVerbId;
}

export const PORTAL_ACTIONS: readonly PortalAction[] = [
  { id: "page-workload", label: "Page a workload", surface: "header", cliVerb: "new" },
  { id: "inbox-list", label: "List and filter Pages", surface: "inbox", cliVerb: "list" },
  { id: "expand-page", label: "Expand a Page card", surface: "card", cliVerb: "get" },
  { id: "view-diff", label: "Read the Proposal diff", surface: "card", cliVerb: "diff" },
  { id: "approve-create-pr", label: "Approve and open the PR", surface: "card", cliVerb: "approve" },
  { id: "dismiss-page", label: "Dismiss a Page", surface: "card", cliVerb: "dismiss" },
  { id: "settings-read", label: "Read masked settings", surface: "settings", cliVerb: "config-get" },
  { id: "settings-write", label: "Save a setting", surface: "settings", cliVerb: "config-set" },
  { id: "settings-test", label: "Test a connection", surface: "settings", cliVerb: "config-test" },
  { id: "settings-unset", label: "Remove a credential", surface: "settings", cliVerb: "config-unset" },
  { id: "sign-in", label: "Sign in with the bootstrap token", surface: "login", cliVerb: "login" },
  { id: "server-version", label: "Read the server version", surface: "footer", cliVerb: "version" },
];

export interface ParityReport {
  ok: boolean;
  // A verb no Portal path performs.
  unpairedVerbs: CliVerbId[];
  // A Portal action with no verb to point at.
  unpairedActions: string[];
  // A verb claimed by more than one Portal path, which is not 50/50.
  duplicatedVerbs: CliVerbId[];
}

export function parityReport(
  verbs: readonly CliVerb[] = CLI_VERBS,
  actions: readonly PortalAction[] = PORTAL_ACTIONS,
): ParityReport {
  const verbIds = new Set(verbs.map((verb) => verb.id));
  const actionVerbIds = new Set<CliVerbId>();

  const unpairedActions = actions.filter((action) => !verbIds.has(action.cliVerb)).map((action) => action.id);

  const duplicatedVerbs = [...verbIds].filter(
    (id) => actions.filter((action) => action.cliVerb === id).length > 1,
  );
  for (const action of actions) {
    if (verbIds.has(action.cliVerb)) {
      actionVerbIds.add(action.cliVerb);
    }
  }
  const unpairedVerbs = [...verbIds].filter((id) => !actionVerbIds.has(id));

  return {
    ok: unpairedVerbs.length === 0 && unpairedActions.length === 0 && duplicatedVerbs.length === 0,
    unpairedVerbs,
    unpairedActions,
    duplicatedVerbs,
  };
}

export interface Command {
  command: string;
  note: string;
}

// The contextual "This Page" tab: every verb that takes a Page, bound to the expanded Page. A Page lives
// in the release namespace, so `-n` (which names the Target) does not apply.
export function thisPageCommands(page: { name: string }): Command[] {
  return [
    { command: `kubectl page get ${page.name}`, note: "The Page, its trace, its Findings, and the pinned Proposal hash." },
    { command: `kubectl page diff ${page.name}`, note: "The stored Proposal, rendered as the diff above." },
    {
      command: `kubectl page approve ${page.name}`,
      note: "Records the approval for the displayed hash; the controller opens the draft PR.",
    },
    {
      command: `kubectl page dismiss ${page.name} --reason "<text>"`,
      note: "Rejects the Proposal without a PR and keeps it in the audit trail.",
    },
  ];
}

export const CLI_GROUPS: readonly CliGroup[] = ["Pages", "Decisions", "Setup"];

export function verbsInGroup(group: CliGroup): CliVerb[] {
  return CLI_VERBS.filter((verb) => verb.group === group);
}
