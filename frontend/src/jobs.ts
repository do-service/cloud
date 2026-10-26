import { notifications } from "@mantine/notifications";
import { ApiError, errorText } from "./api";
import { fetchJob } from "./files";
import { t } from "./i18n";

// How often a background copy is asked about. A variable for tests.
export const jobPoll = { intervalMs: 2000 };

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

// followCopy watches a background copy (E1-6) to its end: a notification
// says it goes on, then how it ended; onDone runs either way, for the
// listing to show the copy. It outlives the page it started on.
export async function followCopy(job: number, name: string, onDone: () => void): Promise<void> {
  const id = `copy-${job}`;
  notifications.show({ id, loading: true, autoClose: false, withCloseButton: false, message: t("actions.copyRunning", { name }) });
  for (;;) {
    await sleep(jobPoll.intervalMs);
    let status;
    try {
      status = await fetchJob(job);
    } catch (err) {
      // Asking failed (offline a moment, say): try again.
      if (err instanceof ApiError && err.status === 404) {
        notifications.update({ id, loading: false, autoClose: 8000, withCloseButton: true, color: "red", message: errorText(err) });
        return;
      }
      continue;
    }
    if (status.state === "running") continue;
    onDone();
    if (status.state === "done") {
      notifications.update({ id, loading: false, autoClose: 4000, withCloseButton: true, message: t("actions.copyDone", { name }) });
    } else {
      const reason = errorText(new ApiError(0, status.error ?? "unknown", "copy failed"));
      notifications.update({ id, loading: false, autoClose: false, withCloseButton: true, color: "red", message: t("actions.copyFailed", { name, reason }) });
    }
    return;
  }
}
