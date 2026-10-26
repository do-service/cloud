import { ActionIcon, Button, Group, Modal, Stack, Table, Text, Tooltip } from "@mantine/core";
import { notifications } from "@mantine/notifications";
import { IconDeviceDesktopOff } from "@tabler/icons-react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { errorText } from "../api";
import type { Device } from "../devices";
import { formatDate, t } from "../i18n";
import { useIsPhone } from "../media";
import { ErrorState, PageLoader } from "./States";

type Props = {
  queryKey: readonly unknown[];
  fetch: (signal: AbortSignal) => Promise<Device[]>;
  revoke: (id: string) => Promise<void>;
};

// Signed-in desktop clients (E12-3) with a revoke each: the user's own in
// /settings, anyone's in the admin's user dialog. The two differ only in
// where the list comes from.
export default function DeviceList({ queryKey, fetch, revoke }: Props) {
  const queryClient = useQueryClient();
  const phone = useIsPhone();
  const [revoking, setRevoking] = useState<Device | null>(null);
  const list = useQuery({ queryKey, queryFn: ({ signal }) => fetch(signal) });
  const remove = useMutation({
    mutationFn: (d: Device) => revoke(d.id),
    onSuccess: (_, d) => {
      setRevoking(null);
      notifications.show({ message: t("devices.revoked", { name: d.name }) });
      void queryClient.invalidateQueries({ queryKey });
    },
    onError: (err) => notifications.show({ color: "red", message: errorText(err) }),
  });

  if (list.isPending) return <PageLoader compact />;
  if (list.data === undefined) return <ErrorState compact error={list.error} onRetry={() => void list.refetch()} />;
  if (list.data.length === 0) {
    return (
      <Text size="sm" c="dimmed">
        {t("devices.none")}
      </Text>
    );
  }
  return (
    <>
      <Table verticalSpacing="xs">
        <Table.Thead>
          <Table.Tr>
            <Table.Th>{t("devices.name")}</Table.Th>
            <Table.Th visibleFrom="sm">{t("devices.connected")}</Table.Th>
            <Table.Th>{t("devices.lastSeen")}</Table.Th>
            <Table.Th w={40} />
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {list.data.map((d) => (
            <Table.Tr key={d.id}>
              {/* As app passwords: on a phone a long name truncates rather
                  than widening the table past the screen. */}
              <Table.Td style={phone ? { maxWidth: 0, width: "100%" } : undefined}>
                <Text size="sm" truncate="end">
                  {d.name}
                </Text>
                <Text size="xs" c="dimmed">
                  {t(`deviceAuth.platforms.${d.platform}`)}
                  {d.clientVersion && ` · ${t("deviceAuth.version", { version: d.clientVersion })}`}
                </Text>
              </Table.Td>
              <Table.Td visibleFrom="sm">
                <Text size="sm">{formatDate(d.createdAt)}</Text>
              </Table.Td>
              <Table.Td>
                <Text size="sm" c={d.lastUsedAt ? undefined : "dimmed"}>
                  {d.lastUsedAt ? formatDate(d.lastUsedAt) : t("devices.neverUsed")}
                </Text>
                {d.lastIp && (
                  <Text size="xs" c="dimmed">
                    {d.lastIp}
                  </Text>
                )}
              </Table.Td>
              <Table.Td>
                <Tooltip label={t("devices.revoke")}>
                  <ActionIcon variant="subtle" color="red" onClick={() => setRevoking(d)} aria-label={t("devices.revokeNamed", { name: d.name })}>
                    <IconDeviceDesktopOff size={16} />
                  </ActionIcon>
                </Tooltip>
              </Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>

      <Modal opened={revoking !== null} onClose={() => setRevoking(null)} title={t("devices.revokeTitle")} fullScreen={phone}>
        {revoking && (
          <Stack>
            <Text size="sm">{t("devices.revokeConfirm", { name: revoking.name })}</Text>
            <Group justify="flex-end">
              <Button variant="default" onClick={() => setRevoking(null)}>
                {t("actions.cancel")}
              </Button>
              <Button color="red" loading={remove.isPending} onClick={() => remove.mutate(revoking)}>
                {t("devices.revoke")}
              </Button>
            </Group>
          </Stack>
        )}
      </Modal>
    </>
  );
}
