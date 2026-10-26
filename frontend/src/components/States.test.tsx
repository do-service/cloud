import { MantineProvider } from "@mantine/core";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { ApiError } from "../api";
import { theme } from "../theme";
import { ErrorState } from "./States";

const failure = new ApiError(500, "internal_error", "boom");

describe("ErrorState", () => {
  it("shows the error's text and retries", async () => {
    const refetch = vi.fn();
    render(
      <MantineProvider theme={theme}>
        <ErrorState error={failure} onRetry={refetch} />
      </MantineProvider>,
    );
    expect(screen.getByText("Не удалось загрузить")).toBeInTheDocument();
    expect(screen.getByText("Внутренняя ошибка сервера.")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Повторить" }));
    expect(refetch).toHaveBeenCalledTimes(1);
  });

  it("has no retry without onRetry, in the compact form too", () => {
    render(
      <MantineProvider theme={theme}>
        <ErrorState compact error={failure} />
      </MantineProvider>,
    );
    expect(screen.getByRole("alert")).toHaveTextContent("Внутренняя ошибка сервера.");
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });
});
