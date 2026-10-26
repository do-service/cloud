import { Anchor, Breadcrumbs, Button, Group, Modal, NavLink, ScrollArea, Stack, Text } from "@mantine/core";
import { IconFolderFilled } from "@tabler/icons-react";
import { useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { fetchChildren, fetchNode, nodeQueryKey, ROOT } from "../files";
import { t } from "../i18n";
import { useIsPhone } from "../media";
import { ErrorState, PageLoader } from "./States";

type Props = {
  opened: boolean;
  // The folder the items are in now: browsing starts there, and moving
  // into it is a no-op.
  from: string;
  // Folders among the items can't be entered: nothing moves into itself.
  movingIds: ReadonlySet<string>;
  onClose: () => void;
  // Reports its own failures; the dialog only waits for it.
  onMove: (targetId: string) => Promise<void>;
  // copy: the items are copied, not moved — into their own folder too,
  // as copies beside them.
  copy?: boolean;
};

// Picks a destination by walking the tree one folder at a time: the
// folder's path on top, its subfolders below, "Move here" at the bottom.
export default function MoveDialog({ opened, from, movingIds, onClose, onMove, copy = false }: Props) {
  const phone = useIsPhone();
  const [at, setAt] = useState(from);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    if (opened) setAt(from);
  }, [opened, from]);

  const node = useQuery({ queryKey: nodeQueryKey(at), queryFn: ({ signal }) => fetchNode(at, signal), enabled: opened });
  // Folders come first in a listing, so the first page holds them unless
  // there are more than a page of folders.
  const folders = useQuery({
    queryKey: ["folders", at],
    queryFn: ({ signal }) => fetchChildren(at, { key: "name", order: "asc" }, null, signal),
    enabled: opened,
  });
  const subfolders = folders.data?.items.filter((n) => n.kind === "dir") ?? [];
  const truncated = folders.data?.nextCursor != null && subfolders.length === folders.data.items.length;
  // "root" and the root's id are the same folder.
  const atId = node.data?.id;
  const fromHere = at === from || (atId !== undefined && atId === from);

  const move = async () => {
    setBusy(true);
    try {
      await onMove(atId ?? at);
      onClose();
    } finally {
      setBusy(false);
    }
  };

  const path = node.data?.path ?? [];
  return (
    <Modal opened={opened} onClose={onClose} title={t(copy ? "actions.copyTitle" : "actions.moveTitle", { count: movingIds.size })} fullScreen={phone}>
      <Stack>
        <Breadcrumbs separatorMargin={4}>
          {path.map((p, i) =>
            i === path.length - 1 ? (
              <Text key={p.id} fw={600} size="sm">
                {p.name || t("files.root")}
              </Text>
            ) : (
              <Anchor key={p.id} size="sm" component="button" type="button" onClick={() => setAt(i === 0 ? ROOT : p.id)}>
                {p.name || t("files.root")}
              </Anchor>
            ),
          )}
        </Breadcrumbs>
        <ScrollArea.Autosize mah={320}>
          {folders.isPending ? (
            <PageLoader compact />
          ) : folders.data === undefined ? (
            <ErrorState compact error={folders.error} onRetry={() => void folders.refetch()} />
          ) : subfolders.length === 0 ? (
            <Text c="dimmed" size="sm" py="md" ta="center">
              {t("actions.noSubfolders")}
            </Text>
          ) : (
            subfolders.map((f) => (
              <NavLink
                key={f.id}
                label={f.name}
                leftSection={<IconFolderFilled size={18} color="var(--mantine-color-yellow-6)" aria-hidden />}
                disabled={movingIds.has(f.id)}
                onClick={() => setAt(f.id)}
              />
            ))
          )}
          {truncated && (
            <Text c="dimmed" size="xs" ta="center" py="xs">
              {t("actions.firstFolders", { count: subfolders.length })}
            </Text>
          )}
        </ScrollArea.Autosize>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>
            {t("actions.cancel")}
          </Button>
          <Button onClick={() => void move()} loading={busy} disabled={(fromHere && !copy) || !node.data}>
            {t(copy ? "actions.copyHere" : "actions.moveHere")}
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}
