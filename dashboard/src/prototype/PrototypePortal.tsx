/**
 * THROWAWAY WAYFINDER PROTOTYPE — ticket 83, branch prototype/v2-shadcn-portal.
 *
 * Question: what should the Pagerless v2.0 portal look like and how should it
 * behave across desktop and mobile? This file mounts three structurally
 * different answers on `/`, switchable with `?variant=A|B|C` and the floating
 * bottom bar. Nothing here is production code: the dataset is in memory, there
 * is no API, no auth, and no persistence. Fold the winning ideas into the real
 * dashboard, then delete this directory.
 */
import { useCallback } from "react";
import { useSearchParams } from "react-router-dom";
import { PrototypeProvider } from "./store";
import { PrototypeSwitcher } from "./PrototypeSwitcher";
import { useModalOpen } from "./use-modal-open";
import { PROTOTYPE_VARIANTS, type VariantKey } from "./variants";
import { PrototypeToaster } from "./shared/toaster";
import { VariantA } from "./VariantA";
import { VariantB } from "./VariantB";
import { VariantC } from "./VariantC";

function readVariant(value: string | null): VariantKey {
  const upper = (value ?? "A").toUpperCase();
  return PROTOTYPE_VARIANTS.some((variant) => variant.key === upper) ? (upper as VariantKey) : "A";
}

export function PrototypePortal() {
  const [searchParams, setSearchParams] = useSearchParams();
  const variant = readVariant(searchParams.get("variant"));
  const modalOpen = useModalOpen();

  const select = useCallback(
    (key: VariantKey) => {
      const next = new URLSearchParams(searchParams);
      next.set("variant", key);
      setSearchParams(next, { replace: true });
    },
    [searchParams, setSearchParams],
  );

  return (
    <PrototypeProvider>
      {variant === "A" && <VariantA />}
      {variant === "B" && <VariantB />}
      {variant === "C" && <VariantC />}
      <PrototypeSwitcher current={variant} onSelect={select} lift={variant === "B"} hidden={modalOpen} />
      <PrototypeToaster />
    </PrototypeProvider>
  );
}
