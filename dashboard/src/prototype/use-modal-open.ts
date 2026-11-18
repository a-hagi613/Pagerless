import { useEffect, useState } from "react";

// Modal surfaces (sheets, dialogs) set `pointer-events: none` on the body while
// they are open. The prototype chrome watches that so it can step aside rather
// than cover a decision footer.
export function useModalOpen(): boolean {
  const [modalOpen, setModalOpen] = useState(() => document.body.style.pointerEvents === "none");

  useEffect(() => {
    const update = () => {
      setModalOpen(document.body.style.pointerEvents === "none");
    };
    update();
    const observer = new MutationObserver(update);
    observer.observe(document.body, { attributes: true, attributeFilter: ["style", "data-scroll-locked"] });
    return () => {
      observer.disconnect();
    };
  }, []);

  return modalOpen;
}
