import { StrictMode } from "react";
import ReactDOM from "react-dom/client";
import "@mantine/core/styles.css";
import { MantineProvider, mergeThemeOverrides } from "@mantine/core";
import { QueryClientProvider } from "@tanstack/react-query";
import { createQueryClient } from "@web/queryClient";
import { theme } from "@web/theme";
import App from "./App";

const rootElement = document.getElementById("root");
if (!rootElement) throw new Error("index.html is missing #root");

const queryClient = createQueryClient();

// The web's stack plus system-ui first: on Linux the stack falls through
// to Helvetica, which fontconfig maps to Nimbus Sans, whose metrics set
// text about 2px above the middle of buttons and inputs; system-ui is the
// desktop's own UI font there, and the same SF and Segoe UI elsewhere.
const fontFamily = "system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif";
const desktopTheme = mergeThemeOverrides(theme, { fontFamily, headings: { fontFamily } });

ReactDOM.createRoot(rootElement).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <MantineProvider theme={desktopTheme} defaultColorScheme="auto">
        <App />
      </MantineProvider>
    </QueryClientProvider>
  </StrictMode>,
);
