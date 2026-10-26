import { afterEach } from "vitest";
import { cleanup, configure } from "@testing-library/react";
import "@testing-library/jest-dom/vitest";

// findBy*/waitFor give up after 1 s by default: on a shared CI runner a
// Mantine menu's open transition alone has taken longer.
configure({ asyncUtilTimeout: 5000 });

// `globals: false` in vite.config.ts means testing-library's own automatic
// afterEach-cleanup registration (which relies on a global `afterEach`)
// never fires — without this, every test's rendered tree stays mounted into
// the next test's, so later tests query against a DOM containing more than
// one copy of the component under test.
afterEach(() => {
  cleanup();
});

// jsdom doesn't implement matchMedia/ResizeObserver, both of which Mantine's
// MantineProvider/ScrollArea touch on mount — polyfilled here once for every
// component test that renders inside a MantineProvider.
if (!window.matchMedia) {
  window.matchMedia = (query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  });
}

// Mantine's Combobox scrolls the highlighted option into view; jsdom has no
// layout, so no scrolling either.
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

if (!window.ResizeObserver) {
  window.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  };
}
