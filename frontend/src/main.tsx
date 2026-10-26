import { StrictMode } from "react";
import ReactDOM from "react-dom/client";
import "@mantine/core/styles.css";
import "@mantine/notifications/styles.css";
import { MantineProvider } from "@mantine/core";
import { Notifications } from "@mantine/notifications";
import { QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router-dom";
import App from "./App";
import { LiveUpdates } from "./live";
import { queryClient } from "./queryClient";
import { theme } from "./theme";

const rootElement = document.getElementById("root");
if (!rootElement) throw new Error("index.html is missing #root");

ReactDOM.createRoot(rootElement).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      {/* Here rather than in App: one per tab, and App's tests run without
          a journal to follow. */}
      <LiveUpdates />
      <MantineProvider theme={theme} defaultColorScheme="auto">
        <Notifications position="top-right" />
        <BrowserRouter>
          <App />
        </BrowserRouter>
      </MantineProvider>
    </QueryClientProvider>
  </StrictMode>,
);
