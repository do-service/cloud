import { createTheme } from "@mantine/core";

// Kept deliberately small until there's a real design: brand color, radius
// and the system font stack (no web-font download on first paint).
export const theme = createTheme({
  primaryColor: "blue",
  defaultRadius: "md",
  fontFamily:
    "-apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif",
});
