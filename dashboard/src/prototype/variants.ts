// The three prototype variants, shared by the portal and the switcher.
export const PROTOTYPE_VARIANTS = [
  { key: "A", name: "Command Sidebar" },
  { key: "B", name: "Split Ops Console" },
  { key: "C", name: "Cluster Matrix" },
] as const;

export type VariantKey = (typeof PROTOTYPE_VARIANTS)[number]["key"];
