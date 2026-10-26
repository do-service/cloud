import { ActionIcon, Badge, Button, CopyButton, Group, PasswordInput, Select, Stack, Text, TextInput, Tooltip } from "@mantine/core";
import { notifications } from "@mantine/notifications";
import { IconCheck, IconCopy, IconLock, IconX } from "@tabler/icons-react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { errorText } from "../api";
import { can, createLink, deleteLink, fetchLinks, linksQueryKey, linkUrl, type FileNode, type LinkMode } from "../files";
import { formatDate, t, type MessageKey } from "../i18n";
import { ErrorState, PageLoader } from "./States";

const MODE_LABELS: Record<LinkMode, MessageKey> = {
  view: "links.modeView",
  download: "links.modeDownload",
  upload: "links.modeUpload",
};

// Expiry choices in days; "" is none.
const EXPIRY_DAYS = ["", "1", "7", "30"] as const;

function expiryLabel(days: string): string {
  return days === "" ? t("links.noExpiry") : t("links.days", { count: days });
}

// Public links of a node (R10): copy or remove the existing ones, make a
// new one with a mode, an optional password and an expiry.
export default function LinksPanel({ node }: { node: FileNode }) {
  const queryClient = useQueryClient();
  const [mode, setMode] = useState<LinkMode>("view");
  const [password, setPassword] = useState("");
  const [days, setDays] = useState<string>("");
  const [busy, setBusy] = useState(false);
  const links = useQuery({ queryKey: linksQueryKey(node.id), queryFn: ({ signal }) => fetchLinks(node.id, signal) });

  // A file drop puts files in a folder, which takes write on it.
  const modes: LinkMode[] = node.kind === "dir" && can(node, "write") ? ["view", "download", "upload"] : ["view", "download"];

  const run = async (action: () => Promise<unknown>, done: string) => {
    setBusy(true);
    try {
      await action();
      notifications.show({ message: done });
    } catch (err) {
      notifications.show({ color: "red", message: errorText(err) });
    } finally {
      setBusy(false);
      void queryClient.invalidateQueries({ queryKey: linksQueryKey(node.id) });
    }
  };

  const create = () =>
    void run(async () => {
      const expiresAt = days === "" ? null : new Date(Date.now() + Number(days) * 24 * 60 * 60 * 1000).toISOString();
      await createLink(node.id, mode, password, expiresAt);
      setPassword("");
    }, t("links.created"));

  return (
    <Stack>
      {links.isPending ? (
        <PageLoader compact />
      ) : links.data === undefined ? (
        <ErrorState compact error={links.error} onRetry={() => void links.refetch()} />
      ) : links.data.length === 0 ? (
        <Text size="sm" c="dimmed">
          {t("links.none")}
        </Text>
      ) : (
        <Stack gap="sm">
          {links.data.map((l) => (
            <Stack key={l.id} gap={4}>
              <Group gap="xs" wrap="nowrap">
                {l.token === null ? (
                  // Sealed under an earlier SECRET_KEY: the link still opens for whoever has it, so it stays here to be revoked.
                  <Text size="sm" c="dimmed" style={{ flex: 1 }}>
                    {t("links.addressLost")}
                  </Text>
                ) : (
                  <>
                    <TextInput value={linkUrl(l.token)} readOnly style={{ flex: 1 }} aria-label={t("links.address")} onFocus={(e) => e.currentTarget.select()} />
                    <CopyButton value={linkUrl(l.token)}>
                      {({ copied, copy }) => (
                        <Tooltip label={copied ? t("links.copied") : t("links.copy")}>
                          <ActionIcon variant="light" size="lg" onClick={copy} aria-label={t("links.copy")}>
                            {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
                          </ActionIcon>
                        </Tooltip>
                      )}
                    </CopyButton>
                  </>
                )}
                <Tooltip label={t("links.delete")}>
                  <ActionIcon
                    variant="subtle"
                    color="gray"
                    size="lg"
                    disabled={busy}
                    onClick={() => void run(() => deleteLink(l.id), t("links.deleted"))}
                    aria-label={t("links.delete")}
                  >
                    <IconX size={16} />
                  </ActionIcon>
                </Tooltip>
              </Group>
              <Group gap="xs">
                <Badge variant="light">{t(MODE_LABELS[l.mode])}</Badge>
                {l.hasPassword && (
                  <Badge variant="light" color="gray" leftSection={<IconLock size={12} />}>
                    {t("links.withPassword")}
                  </Badge>
                )}
                <Text size="xs" c="dimmed">
                  {l.expiresAt ? t("links.until", { date: formatDate(l.expiresAt) }) : t("links.noExpiry")}
                </Text>
              </Group>
            </Stack>
          ))}
        </Stack>
      )}

      <Text fw={600} size="sm" mt="sm">
        {t("links.new")}
      </Text>
      <Group grow align="flex-end">
        <Select
          label={t("links.mode")}
          data={modes.map((m) => ({ value: m, label: t(MODE_LABELS[m]) }))}
          value={mode}
          onChange={(v) => setMode(modes.find((m) => m === v) ?? "view")}
          allowDeselect={false}
        />
        <Select
          label={t("links.expiry")}
          data={EXPIRY_DAYS.map((d) => ({ value: d, label: expiryLabel(d) }))}
          value={days}
          onChange={(v) => setDays(v ?? "")}
          allowDeselect={false}
        />
      </Group>
      <PasswordInput label={t("links.password")} description={t("links.passwordHint")} value={password} onChange={(e) => setPassword(e.currentTarget.value)} autoComplete="new-password" />
      <Group justify="flex-end">
        <Button onClick={create} loading={busy}>
          {t("links.create")}
        </Button>
      </Group>
    </Stack>
  );
}
