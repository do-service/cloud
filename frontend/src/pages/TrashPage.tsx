import { Button, Center, Checkbox, Group, Modal, Stack, Table, Text, Title } from "@mantine/core";
import { notifications } from "@mantine/notifications";
import { IconArrowBackUp, IconFolderFilled, IconTrash, IconTrashX } from "@tabler/icons-react";
import { useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { errorText } from "../api";
import { EmptyState, ErrorState, PageLoader } from "../components/States";
import { emptyTrash, fetchTrash, purgeNode, restoreNode, trashQueryKey, type TrashItem } from "../files";
import { formatBytes, formatDate, t } from "../i18n";
import { useIsPhone } from "../media";

type Confirm = { kind: "purge"; items: TrashItem[] } | { kind: "empty" } | null;

// The trash (E9-6): what was deleted, from where and when; restore puts it
// back (renamed if its place is taken), purge deletes it for good.
export default function TrashPage() {
  const queryClient = useQueryClient();
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [confirm, setConfirm] = useState<Confirm>(null);
  const [busy, setBusy] = useState(false);
  const phone = useIsPhone();

  const trash = useInfiniteQuery({
    queryKey: trashQueryKey,
    queryFn: ({ pageParam, signal }) => fetchTrash(pageParam || null, signal),
    initialPageParam: "",
    getNextPageParam: (last) => last.nextCursor ?? undefined,
  });
  const items = trash.data?.pages.flatMap((p) => p.items) ?? [];
  const selection = items.filter((n) => selected.has(n.id));

  // Restoring and purging change folders, sizes and the quota too.
  const refresh = () => {
    setSelected(new Set());
    void queryClient.invalidateQueries({ queryKey: trashQueryKey });
    void queryClient.invalidateQueries({ queryKey: ["children"] });
    void queryClient.invalidateQueries({ queryKey: ["node"] });
  };

  // Runs act on each item on its own, so one failure doesn't stop the rest,
  // then says how it went.
  const each = async (list: TrashItem[], act: (id: string) => Promise<unknown>, done: (count: number) => string) => {
    setBusy(true);
    const results = await Promise.allSettled(list.map((n) => act(n.id)));
    setBusy(false);
    const failed = results.filter((r) => r.status === "rejected");
    const ok = results.length - failed.length;
    if (failed.length === 0) notifications.show({ message: done(ok) });
    else notifications.show({ color: "red", title: ok > 0 ? done(ok) : undefined, message: errorText(failed[0].reason) });
    refresh();
  };

  const toggle = (id: string) => {
    const next = new Set(selected);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    setSelected(next);
  };
  const allSelected = items.length > 0 && selection.length === items.length;

  // A failed next page or refetch keeps the loaded list; only a trash that
  // never loaded is replaced by the error.
  if (trash.isError && trash.data === undefined) {
    return <ErrorState error={trash.error} onRetry={() => void trash.refetch()} />;
  }
  // Its retry is the same "more" request, so it takes the button's place.
  const moreFailed = trash.isFetchNextPageError && !trash.isFetchingNextPage;
  return (
    <Stack gap="sm">
      <Group justify="space-between">
        <Title order={3}>{t("trash.title")}</Title>
        <Group gap="xs">
          {selection.length > 0 && (
            <>
              <Button
                variant="default"
                size="xs"
                leftSection={<IconArrowBackUp size={16} />}
                loading={busy}
                onClick={() => void each(selection, restoreNode, (count) => t("trash.restored", { count }))}
              >
                {t("trash.restore")}
              </Button>
              <Button variant="light" color="red" size="xs" leftSection={<IconTrashX size={16} />} onClick={() => setConfirm({ kind: "purge", items: selection })}>
                {t("trash.purge")}
              </Button>
            </>
          )}
          {items.length > 0 && selection.length === 0 && (
            <Button variant="light" color="red" size="xs" leftSection={<IconTrashX size={16} />} onClick={() => setConfirm({ kind: "empty" })}>
              {t("trash.empty")}
            </Button>
          )}
        </Group>
      </Group>
      <Text size="sm" c="dimmed">
        {t("trash.hint")}
      </Text>

      {trash.isPending ? (
        <PageLoader />
      ) : items.length === 0 ? (
        <EmptyState icon={<IconTrash size={36} stroke={1.5} />} color="gray" title={t("trash.emptyTitle")} />
      ) : (
        <Table highlightOnHover verticalSpacing="xs">
          <Table.Thead>
            <Table.Tr>
              <Table.Th w={40}>
                <Checkbox
                  size="xs"
                  aria-label={t("files.selectAll")}
                  checked={allSelected}
                  indeterminate={selection.length > 0 && !allSelected}
                  onChange={() => setSelected(allSelected ? new Set() : new Set(items.map((n) => n.id)))}
                />
              </Table.Th>
              <Table.Th>{t("files.columns.name")}</Table.Th>
              {/* A phone shows the size and the date under the name. */}
              {!phone && <Table.Th visibleFrom="sm">{t("trash.from")}</Table.Th>}
              {!phone && (
                <Table.Th visibleFrom="xs" ta="right">
                  {t("files.columns.size")}
                </Table.Th>
              )}
              {!phone && <Table.Th ta="right">{t("trash.deletedAt")}</Table.Th>}
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {items.map((n) => (
              <Table.Tr key={n.id} bg={selected.has(n.id) ? "var(--mantine-primary-color-light)" : undefined}>
                <Table.Td>
                  <Checkbox size="xs" aria-label={t("files.select", { name: n.name })} checked={selected.has(n.id)} onChange={() => toggle(n.id)} />
                </Table.Td>
                {/* No width of its own on a phone: the name truncates
                    instead of widening the table past the screen. */}
                <Table.Td style={phone ? { maxWidth: 0, width: "100%" } : undefined}>
                  <Group gap="xs" wrap="nowrap">
                    {n.kind === "dir" && <IconFolderFilled size={18} color="var(--mantine-color-yellow-6)" aria-hidden />}
                    <Text size="sm" truncate="end">
                      {n.name}
                    </Text>
                  </Group>
                  {phone && (
                    <Text size="xs" c="dimmed" truncate="end">
                      {formatBytes(n.size)} · {formatDate(n.trashedAt)}
                    </Text>
                  )}
                </Table.Td>
                {!phone && (
                  <>
                    <Table.Td visibleFrom="sm">
                      <Text size="sm" c="dimmed" truncate="end">
                        {n.originalPath || t("files.root")}
                      </Text>
                    </Table.Td>
                    <Table.Td visibleFrom="xs" ta="right">
                      <Text size="sm" c="dimmed">
                        {formatBytes(n.size)}
                      </Text>
                    </Table.Td>
                    <Table.Td ta="right">
                      <Text size="sm" c="dimmed">
                        {formatDate(n.trashedAt)}
                      </Text>
                    </Table.Td>
                  </>
                )}
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      )}
      {moreFailed && <ErrorState compact error={trash.error} onRetry={() => void trash.fetchNextPage()} />}
      {trash.hasNextPage && !moreFailed && (
        <Center>
          <Button variant="subtle" loading={trash.isFetchingNextPage} onClick={() => void trash.fetchNextPage()}>
            {t("trash.more")}
          </Button>
        </Center>
      )}

      <Modal
        opened={confirm !== null}
        onClose={() => setConfirm(null)}
        title={confirm?.kind === "empty" ? t("trash.emptyConfirmTitle") : t("trash.purgeConfirmTitle")}
        fullScreen={phone}
      >
        <Stack>
          <Text size="sm">
            {confirm?.kind === "empty" ? t("trash.emptyConfirm") : t("trash.purgeConfirm", { count: confirm?.kind === "purge" ? confirm.items.length : 0 })}
          </Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setConfirm(null)}>
              {t("actions.cancel")}
            </Button>
            <Button
              color="red"
              loading={busy}
              onClick={async () => {
                if (confirm?.kind === "purge") await each(confirm.items, purgeNode, (count) => t("trash.purged", { count }));
                else {
                  setBusy(true);
                  try {
                    await emptyTrash();
                    notifications.show({ message: t("trash.emptied") });
                  } catch (err) {
                    notifications.show({ color: "red", message: errorText(err) });
                  }
                  setBusy(false);
                  refresh();
                }
                setConfirm(null);
              }}
            >
              {t("trash.purgeForever")}
            </Button>
          </Group>
        </Stack>
      </Modal>
    </Stack>
  );
}
