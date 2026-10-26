import { useMediaQuery } from "@mantine/hooks";

// Below Mantine's sm breakpoint (48em), as its hiddenFrom="sm" draws the
// line: a phone, which gets a layout of its own rather than a squeezed one.
export const PHONE_MEDIA = "(max-width: 47.99375em)";

// A finger, not a mouse: what can't be hovered or right-clicked gets a
// button of its own.
export const COARSE_MEDIA = "(pointer: coarse)";

// Read at once, not after mount, so the desktop layout never flashes first.
const NOW = { getInitialValueInEffect: false };

export function useIsPhone(): boolean {
  return useMediaQuery(PHONE_MEDIA, false, NOW);
}

export function useCoarsePointer(): boolean {
  return useMediaQuery(COARSE_MEDIA, false, NOW);
}
