import { MantineProvider } from "@mantine/core";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Link, MemoryRouter, Route, Routes } from "react-router-dom";
import { theme } from "../theme";
import ErrorBoundary from "./ErrorBoundary";

function Broken(): never {
  throw new Error("render failed");
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("ErrorBoundary", () => {
  it("shows a way out instead of a blank page, and lets the next page render", async () => {
    // React and the boundary both log the error; expected here.
    const logged = vi.spyOn(console, "error").mockImplementation(() => {});
    render(
      <MantineProvider theme={theme}>
        <MemoryRouter initialEntries={["/broken"]}>
          <Link to="/fine">away</Link>
          <ErrorBoundary>
            <Routes>
              <Route path="/broken" element={<Broken />} />
              <Route path="/fine" element={<p>fine</p>} />
            </Routes>
          </ErrorBoundary>
        </MemoryRouter>
      </MantineProvider>,
    );
    expect(screen.getByText("Что-то пошло не так")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Обновить страницу" })).toBeInTheDocument();
    expect(logged).toHaveBeenCalled();

    await userEvent.click(screen.getByRole("link", { name: "away" }));
    expect(screen.getByText("fine")).toBeInTheDocument();
    expect(screen.queryByText("Что-то пошло не так")).not.toBeInTheDocument();
  });
});
