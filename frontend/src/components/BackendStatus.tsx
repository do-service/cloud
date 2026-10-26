import { Badge, Tooltip } from "@mantine/core";
import { useQuery } from "@tanstack/react-query";
import { checkReady, errorText } from "../api";
import { t } from "../i18n";

const POLL_INTERVAL_MS = 30_000;

// Header indicator of whether the backend is reachable and ready. Polls
// /readyz rather than /healthz: "alive but still migrating" should read as
// unavailable to the user, since requests would fail anyway.
export default function BackendStatus() {
  const { isPending, isError, error } = useQuery({
    queryKey: ["readyz"],
    // TanStack Query rejects `undefined` as query data, and the probe has
    // no payload — success itself is the datum.
    queryFn: async ({ signal }) => {
      await checkReady(signal);
      return true;
    },
    refetchInterval: POLL_INTERVAL_MS,
    // A failed probe is itself the answer; retrying would only delay the
    // red state. The next poll is the retry.
    retry: false,
  });

  // isError wins over stale success data: after a failed refetch TanStack
  // keeps the old result around, but the backend is down *now*.
  if (isError) {
    return (
      <Tooltip label={errorText(error)}>
        <Badge variant="dot" color="red">
          {t("status.unavailable")}
        </Badge>
      </Tooltip>
    );
  }
  if (isPending) {
    return (
      <Badge variant="dot" color="gray">
        {t("status.checking")}
      </Badge>
    );
  }
  return (
    <Badge variant="dot" color="green">
      {t("status.available")}
    </Badge>
  );
}
