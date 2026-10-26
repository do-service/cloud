import { StrictMode } from "react";
import ReactDOM from "react-dom/client";
import "@mantine/core/styles.css";
import { MantineProvider } from "@mantine/core";
import { QueryClientProvider } from "@tanstack/react-query";
import { createQueryClient } from "@web/queryClient";
import { theme } from "@web/theme";
import App from "./App";

const rootElement = document.getElementById("root");
if (!rootElement) throw new Error("index.html is missing #root");

const queryClient = createQueryClient();

ReactDOM.createRoot(rootElement).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <MantineProvider theme={theme} defaultColorScheme="auto">
        <App />
      </MantineProvider>
    </QueryClientProvider>
  </StrictMode>,
);
