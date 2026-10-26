import {
  Accordion,
  ActionIcon,
  Alert,
  Button,
  Code,
  CopyButton,
  Group,
  Modal,
  Paper,
  Progress,
  Stack,
  Table,
  Text,
  TextInput,
  Title,
  Tooltip,
} from "@mantine/core";
import { notifications } from "@mantine/notifications";
import { IconCheck, IconCopy, IconKey, IconTrash } from "@tabler/icons-react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import {
  appPasswordsQueryKey,
  createAppPassword,
  deleteAppPassword,
  fetchAppPasswords,
  fetchQuota,
  quotaQueryKey,
  webdavUrl,
  type AppPassword,
} from "../account";
import { errorText } from "../api";
import { fetchMe, meQueryKey } from "../auth";
import DeviceList from "../components/DeviceList";
import { ErrorState, PageLoader } from "../components/States";
import { devicesQueryKey, fetchDevices, revokeDevice } from "../devices";
import { formatBytes, formatDate, t } from "../i18n";
import { useIsPhone } from "../media";

// The account's settings (E9-10): space used, connecting a drive over
// WebDAV, the app passwords that drive signs in with, and the desktop
// clients signed in (E12-3).
export default function SettingsPage() {
  const me = useQuery({ queryKey: meQueryKey, queryFn: ({ signal }) => fetchMe(signal), staleTime: Infinity, retry: false });
  return (
    <Stack gap="lg" maw={760}>
      <Title order={3}>{t("settings.title")}</Title>
      <QuotaSection />
      {me.data && <DriveSection username={me.data.username} />}
      <AppPasswordsSection />
      <DevicesSection />
    </Stack>
  );
}

function CopyField({ label, value }: { label: string; value: string }) {
  return (
    <Group gap="xs" wrap="nowrap" align="flex-end">
      <TextInput label={label} value={value} readOnly style={{ flex: 1 }} onFocus={(e) => e.currentTarget.select()} />
      <CopyButton value={value}>
        {({ copied, copy }) => (
          <Tooltip label={copied ? t("links.copied") : t("links.copy")}>
            <ActionIcon variant="light" size="lg" onClick={copy} aria-label={t("settings.copy", { what: label })}>
              {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
            </ActionIcon>
          </Tooltip>
        )}
      </CopyButton>
    </Group>
  );
}

function QuotaSection() {
  const quota = useQuery({ queryKey: quotaQueryKey, queryFn: ({ signal }) => fetchQuota(signal) });
  return (
    <Paper withBorder p="md">
      <Stack gap="xs">
        <Title order={4}>{t("settings.quotaTitle")}</Title>
        {quota.isPending ? (
          <PageLoader compact />
        ) : quota.data === undefined ? (
          <ErrorState compact error={quota.error} onRetry={() => void quota.refetch()} />
        ) : quota.data.quotaBytes === null ? (
          <Text size="sm">{t("settings.quotaUnlimited", { used: formatBytes(quota.data.usedBytes) })}</Text>
        ) : (
          <>
            <Progress
              value={quota.data.quotaBytes > 0 ? Math.min((quota.data.usedBytes / quota.data.quotaBytes) * 100, 100) : 100}
              color={quota.data.usedBytes >= quota.data.quotaBytes * 0.9 ? "red" : undefined}
              aria-label={t("settings.quotaTitle")}
            />
            <Text size="sm">{t("settings.quotaUsed", { used: formatBytes(quota.data.usedBytes), total: formatBytes(quota.data.quotaBytes) })}</Text>
          </>
        )}
        <Text size="xs" c="dimmed">
          {t("settings.quotaHint")}
        </Text>
      </Stack>
    </Paper>
  );
}

function DriveSection({ username }: { username: string }) {
  const url = webdavUrl(username);
  return (
    <Paper withBorder p="md">
      <Stack gap="sm">
        <Title order={4}>{t("settings.driveTitle")}</Title>
        <Text size="sm">{t("settings.driveHint")}</Text>
        <CopyField label={t("settings.driveUrl")} value={url} />
        <CopyField label={t("settings.driveUser")} value={username} />
        <Accordion variant="separated">
          <Accordion.Item value="windows">
            <Accordion.Control>Windows</Accordion.Control>
            <Accordion.Panel>
              <Text size="sm">{t("settings.howWindows")}</Text>
            </Accordion.Panel>
          </Accordion.Item>
          <Accordion.Item value="macos">
            <Accordion.Control>macOS</Accordion.Control>
            <Accordion.Panel>
              <Text size="sm">{t("settings.howMac")}</Text>
            </Accordion.Panel>
          </Accordion.Item>
          <Accordion.Item value="linux">
            <Accordion.Control>{t("settings.linuxTitle")}</Accordion.Control>
            <Accordion.Panel>
              <Stack gap="xs">
                <Text size="sm">{t("settings.howLinux")}</Text>
                <Code block>{`rclone config create cloud webdav url=${url} vendor=other user=${username} pass=${t("settings.appPasswordPlaceholder")} --obscure`}</Code>
              </Stack>
            </Accordion.Panel>
          </Accordion.Item>
          <Accordion.Item value="phone">
            <Accordion.Control>{t("settings.phoneTitle")}</Accordion.Control>
            <Accordion.Panel>
              <Text size="sm">{t("settings.howPhone")}</Text>
            </Accordion.Panel>
          </Accordion.Item>
        </Accordion>
      </Stack>
    </Paper>
  );
}

function DevicesSection() {
  return (
    <Paper withBorder p="md">
      <Stack gap="sm">
        <Title order={4}>{t("devices.title")}</Title>
        <Text size="sm">{t("devices.hint")}</Text>
        <DeviceList queryKey={devicesQueryKey} fetch={fetchDevices} revoke={revokeDevice} />
      </Stack>
    </Paper>
  );
}

function AppPasswordsSection() {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [created, setCreated] = useState<{ name: string; password: string } | null>(null);
  const [removing, setRemoving] = useState<AppPassword | null>(null);
  const phone = useIsPhone();
  const list = useQuery({ queryKey: appPasswordsQueryKey, queryFn: ({ signal }) => fetchAppPasswords(signal) });
  const refresh = () => void queryClient.invalidateQueries({ queryKey: appPasswordsQueryKey });

  const create = useMutation({
    mutationFn: (n: string) => createAppPassword(n),
    onSuccess: (password, n) => {
      setCreated({ name: n, password });
      setName("");
      refresh();
    },
  });
  const remove = useMutation({
    mutationFn: (p: AppPassword) => deleteAppPassword(p.id),
    onSuccess: () => {
      setRemoving(null);
      notifications.show({ message: t("settings.passwordDeleted") });
      refresh();
    },
    onError: (err) => notifications.show({ color: "red", message: errorText(err) }),
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (name.trim()) create.mutate(name.trim());
  };

  return (
    <Paper withBorder p="md">
      <Stack gap="sm">
        <Title order={4}>{t("settings.passwordsTitle")}</Title>
        <Text size="sm">{t("settings.passwordsHint")}</Text>
        <form onSubmit={submit}>
          <Group align="flex-end" gap="xs">
            <TextInput
              label={t("settings.passwordName")}
              placeholder={t("settings.passwordNamePlaceholder")}
              value={name}
              onChange={(e) => setName(e.currentTarget.value)}
              maxLength={100}
              style={{ flex: 1 }}
            />
            <Button type="submit" leftSection={<IconKey size={16} />} loading={create.isPending} disabled={!name.trim()}>
              {t("settings.passwordCreate")}
            </Button>
          </Group>
        </form>
        {create.isError && <Alert color="red">{errorText(create.error)}</Alert>}

        {list.isPending ? (
          <PageLoader compact />
        ) : list.data === undefined ? (
          <ErrorState compact error={list.error} onRetry={() => void list.refetch()} />
        ) : list.data.length === 0 ? (
          <Text size="sm" c="dimmed">
            {t("settings.passwordsNone")}
          </Text>
        ) : (
          <Table verticalSpacing="xs">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t("settings.passwordName")}</Table.Th>
                <Table.Th visibleFrom="sm">{t("settings.passwordCreated")}</Table.Th>
                <Table.Th>{t("settings.passwordLastUsed")}</Table.Th>
                <Table.Th w={40} />
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {list.data.map((p) => (
                <Table.Tr key={p.id}>
                  {/* No width of its own on a phone: a long name truncates
                      instead of widening the table past the screen. */}
                  <Table.Td style={phone ? { maxWidth: 0, width: "100%" } : undefined}>
                    <Text size="sm" truncate="end">
                      {p.name}
                    </Text>
                  </Table.Td>
                  <Table.Td visibleFrom="sm">
                    <Text size="sm">{formatDate(p.createdAt)}</Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="sm" c={p.lastUsedAt ? undefined : "dimmed"}>
                      {p.lastUsedAt ? formatDate(p.lastUsedAt) : t("settings.passwordNeverUsed")}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Tooltip label={t("settings.passwordDelete")}>
                      <ActionIcon variant="subtle" color="red" onClick={() => setRemoving(p)} aria-label={t("settings.passwordDeleteNamed", { name: p.name })}>
                        <IconTrash size={16} />
                      </ActionIcon>
                    </Tooltip>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        )}
      </Stack>

      <Modal opened={created !== null} onClose={() => setCreated(null)} title={t("settings.passwordCreatedTitle", { name: created?.name ?? "" })} fullScreen={phone}>
        {created && (
          <Stack>
            <Alert color="yellow">{t("settings.passwordOnce")}</Alert>
            <CopyField label={t("settings.passwordLabel")} value={created.password} />
            <Group justify="flex-end">
              <Button onClick={() => setCreated(null)}>{t("settings.passwordDone")}</Button>
            </Group>
          </Stack>
        )}
      </Modal>

      <Modal opened={removing !== null} onClose={() => setRemoving(null)} title={t("settings.passwordDeleteTitle")} fullScreen={phone}>
        {removing && (
          <Stack>
            <Text size="sm">{t("settings.passwordDeleteConfirm", { name: removing.name })}</Text>
            <Group justify="flex-end">
              <Button variant="default" onClick={() => setRemoving(null)}>
                {t("actions.cancel")}
              </Button>
              <Button color="red" loading={remove.isPending} onClick={() => remove.mutate(removing)}>
                {t("settings.passwordDelete")}
              </Button>
            </Group>
          </Stack>
        )}
      </Modal>
    </Paper>
  );
}
