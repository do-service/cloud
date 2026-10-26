import { Badge, Group, Paper, Stack, Table, Text, Title } from "@mantine/core";
import { useQuery } from "@tanstack/react-query";
import type { CSSProperties } from "react";
import { fetchSystem, systemQueryKey } from "../../admin";
import { formatBytes, t } from "../../i18n";
import { useIsPhone } from "../../media";
import { ErrorState, PageLoader } from "../States";

// What the system runs with. Set through the environment (docs, compose),
// so shown here, not edited.
export default function SystemTab() {
  const system = useQuery({ queryKey: systemQueryKey, queryFn: ({ signal }) => fetchSystem(signal) });
  // Addresses have no spaces to wrap at: on a phone they break anywhere.
  const url: CSSProperties | undefined = useIsPhone() ? { overflowWrap: "anywhere" } : undefined;
  if (system.isPending) return <PageLoader />;
  if (system.data === undefined) return <ErrorState compact error={system.error} onRetry={() => void system.refetch()} />;
  const s = system.data;
  const editor = !s.editor.configured ? "off" : s.editor.available ? "on" : "down";
  const editorLook = { on: "green", down: "red", off: "gray" } as const;
  return (
    <Stack gap="md" maw={760}>
      <Text size="sm" c="dimmed">
        {t("admin.system.hint")}
      </Text>
      <Paper withBorder p="md">
        <Stack gap="xs">
          <Title order={5}>{t("admin.system.signIn")}</Title>
          {s.passwordLogin && <Text size="sm">{t("admin.system.passwordLogin")}</Text>}
          {s.providers.length === 0 ? (
            <Text size="sm" c="dimmed">
              {t("admin.system.noProviders")}
            </Text>
          ) : (
            <Table verticalSpacing={4}>
              <Table.Tbody>
                {s.providers.map((p) => (
                  <Table.Tr key={p.slug}>
                    <Table.Td>
                      <Text size="sm" fw={500}>
                        {p.name}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Text size="xs" c="dimmed" style={url}>
                        {p.issuer}
                      </Text>
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          )}
        </Stack>
      </Paper>
      <Paper withBorder p="md">
        <Stack gap="xs">
          <Group justify="space-between">
            <Title order={5}>{t("admin.system.editor")}</Title>
            <Badge variant="light" color={editorLook[editor]}>
              {t(`admin.system.editorStatus.${editor}`)}
            </Badge>
          </Group>
          {s.editor.url && (
            <Text size="xs" c="dimmed" style={url}>
              {s.editor.url}
            </Text>
          )}
        </Stack>
      </Paper>
      <Paper withBorder p="md">
        <Stack gap={4}>
          <Title order={5}>{t("admin.system.policies")}</Title>
          <Text size="sm">
            {t("admin.system.defaultQuota", { value: s.defaultQuotaBytes === null ? t("admin.system.unlimited") : formatBytes(s.defaultQuotaBytes) })}
          </Text>
          <Text size="sm">
            {t("admin.system.trash", { value: s.trashRetentionDays > 0 ? t("admin.system.days", { count: s.trashRetentionDays }) : t("admin.system.forever") })}
          </Text>
          <Text size="sm">
            {t("admin.system.audit", { value: s.auditRetentionDays > 0 ? t("admin.system.days", { count: s.auditRetentionDays }) : t("admin.system.forever") })}
          </Text>
          <Text size="sm" c="dimmed">
            {t("admin.system.version", { version: s.version })}
          </Text>
        </Stack>
      </Paper>
    </Stack>
  );
}
