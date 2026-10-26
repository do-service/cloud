import { ActionIcon, Badge, Box, Group, Modal, Stack, Text, Tooltip } from "@mantine/core";
import { notifications } from "@mantine/notifications";
import { IconArrowBackUp, IconDownload } from "@tabler/icons-react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { errorText } from "../api";
import { can, contentUrl, download, fetchVersions, restoreVersion, versionContentUrl, versionsQueryKey, type Author, type FileNode } from "../files";
import { formatBytes, formatDate, t } from "../i18n";
import { useIsPhone } from "../media";
import { ErrorState, PageLoader } from "./States";

type Props = { file: FileNode | null; onClose: () => void };

// Versions saved within a minute of each other are common: seconds tell
// them apart.
const withSeconds: Intl.DateTimeFormatOptions = { dateStyle: "medium", timeStyle: "medium" };

// When some content was saved, and by whom where that's known.
function Saved({ at, author }: { at: string; author: Author | undefined }) {
  return (
    <Text size="sm">
      {formatDate(at, withSeconds)}
      {author && (
        <Text span size="sm" c="dimmed">
          {` · ${author.displayName}`}
        </Text>
      )}
    </Text>
  );
}

// A file's history (R7): the current content on top, earlier ones below,
// each downloadable; restoring one makes it current and keeps the rest.
export default function VersionsDialog({ file, onClose }: Props) {
  const phone = useIsPhone();
  const queryClient = useQueryClient();
  const [restoring, setRestoring] = useState<string | null>(null);
  const versions = useQuery({
    queryKey: versionsQueryKey(file?.id ?? ""),
    queryFn: ({ signal }) => fetchVersions(file?.id ?? "", signal),
    enabled: file !== null,
  });

  const restore = async (id: string) => {
    setRestoring(id);
    try {
      await restoreVersion(id);
      notifications.show({ message: t("versions.restored") });
      void queryClient.invalidateQueries({ queryKey: ["children"] });
      void queryClient.invalidateQueries({ queryKey: ["node"] });
      void queryClient.invalidateQueries({ queryKey: ["versions"] });
      onClose();
    } catch (err) {
      notifications.show({ color: "red", message: errorText(err) });
    } finally {
      setRestoring(null);
    }
  };

  return (
    <Modal opened={file !== null} onClose={onClose} title={t("versions.title", { name: file?.name ?? "" })} size="lg" fullScreen={phone}>
      {file && (
        <Stack gap={0}>
          <Group justify="space-between" wrap="nowrap" py="xs">
            <Group gap="xs">
              <Saved at={file.mtime} author={versions.data?.currentAuthor} />
              <Badge size="sm" variant="light">
                {t("versions.current")}
              </Badge>
            </Group>
            <Group gap="xs" wrap="nowrap">
              <Text size="sm" c="dimmed">
                {formatBytes(file.size)}
              </Text>
              <Tooltip label={t("actions.download")}>
                <ActionIcon variant="subtle" aria-label={t("versions.downloadCurrent")} onClick={() => download(contentUrl(file.id))}>
                  <IconDownload size={16} />
                </ActionIcon>
              </Tooltip>
              {/* Where the rows below have their restore button. */}
              <Box w={28} />
            </Group>
          </Group>
          {versions.isPending ? (
            <PageLoader compact />
          ) : versions.data === undefined ? (
            <ErrorState compact error={versions.error} onRetry={() => void versions.refetch()} />
          ) : versions.data.items.length === 0 ? (
            <Text size="sm" c="dimmed" py="md" ta="center">
              {t("versions.none")}
            </Text>
          ) : (
            versions.data.items.map((v) => (
              <Group key={v.id} justify="space-between" wrap="nowrap" py="xs" style={{ borderTop: "1px solid var(--mantine-color-default-border)" }}>
                <Saved at={v.createdAt} author={v.author} />
                <Group gap="xs" wrap="nowrap">
                  <Text size="sm" c="dimmed">
                    {formatBytes(v.size)}
                  </Text>
                  <Tooltip label={t("actions.download")}>
                    <ActionIcon variant="subtle" aria-label={t("versions.download", { date: formatDate(v.createdAt, withSeconds) })} onClick={() => download(versionContentUrl(v.id))}>
                      <IconDownload size={16} />
                    </ActionIcon>
                  </Tooltip>
                  {can(file, "write") && (
                    <Tooltip label={t("versions.restore")}>
                      <ActionIcon
                        variant="subtle"
                        aria-label={t("versions.restoreFrom", { date: formatDate(v.createdAt, withSeconds) })}
                        loading={restoring === v.id}
                        disabled={restoring !== null}
                        onClick={() => void restore(v.id)}
                      >
                        <IconArrowBackUp size={16} />
                      </ActionIcon>
                    </Tooltip>
                  )}
                </Group>
              </Group>
            ))
          )}
        </Stack>
      )}
    </Modal>
  );
}
