import { ActionIcon, Alert, Checkbox, Group, Stack, Text, Title, Tooltip } from "@mantine/core";
import { IconAlertTriangle, IconArrowLeft, IconCircleCheck, IconClock } from "@tabler/icons-react";
import { keepPreviousData, useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useMemo, useState } from "react";
import { ApiError } from "@web/api";
import FileList from "@web/components/FileList";
import { ErrorState, PageLoader } from "@web/components/States";
import {
  childrenQueryKey,
  fetchChildren,
  fetchSharedWithMe,
  ROOT,
  sharedWithMeQueryKey,
  type FileNode,
  type Sort,
  type SortKey,
} from "@web/files";
import { formatBytes, t } from "@web/i18n";
import { previewable, previewUrl } from "@web/preview";
import {
  appErrorText,
  fetchMarks,
  fetchUsage,
  marksQueryKey,
  NoSpaceError,
  setChecked,
  usageQueryKey,
  type Mark,
} from "./appApi";

// The engine's id of «Доступно мне» (internal/engine SharedID): a folder
// of the sync folder, not a node of the server, which lists what is
// shared at /shared-with-me instead.
export const SHARED = "~shared";

type Folder = { id: string; name: string };

// How often the checkboxes and statuses follow the engine.
const MARKS_EVERY = 2000;

function byName(a: FileNode, b: FileNode, sort: Sort): number {
  const dir = sort.order === "asc" ? 1 : -1;
  if (a.kind !== b.kind) return a.kind === "dir" ? -1 : 1;
  switch (sort.key) {
    case "size":
      return (a.size - b.size) * dir;
    case "mtime":
      return a.mtime.localeCompare(b.mtime) * dir;
  }
  return a.name.localeCompare(b.name, "ru") * dir;
}

// The cloud of one connection: the server's tree (own and «Доступно
// мне») with a checkbox per item — what syncs to this computer — and its
// sync status, through the web's FileList.
export default function Browser({ connId, rootName }: { connId: string; rootName: string }) {
  const queryClient = useQueryClient();
  const [path, setPath] = useState<Folder[]>([{ id: ROOT, name: rootName }]);
  const [sort, setSort] = useState<Sort>({ key: "name", order: "asc" });
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [failure, setFailure] = useState<string | null>(null);
  const folder = path[path.length - 1];
  const inShared = folder.id === SHARED;

  const children = useInfiniteQuery({
    queryKey: childrenQueryKey(folder.id, sort),
    // "" is the first page: the backend takes no cursor for it.
    queryFn: ({ pageParam, signal }) => fetchChildren(folder.id, sort, pageParam || null, signal),
    initialPageParam: "",
    getNextPageParam: (last) => last.nextCursor ?? undefined,
    enabled: !inShared,
  });
  // At the root, for its «Доступно мне» row; inside it, its items.
  const shared = useQuery({
    queryKey: sharedWithMeQueryKey,
    queryFn: ({ signal }) => fetchSharedWithMe(signal),
    enabled: inShared || folder.id === ROOT,
  });
  const { hasNextPage, isFetchingNextPage, isFetchNextPageError, fetchNextPage } = children;
  // After a failed next page only the user's retry asks again (see FileList).
  const loadMore = useCallback(() => {
    if (hasNextPage && !isFetchingNextPage && !isFetchNextPageError) void fetchNextPage();
  }, [hasNextPage, isFetchingNextPage, isFetchNextPageError, fetchNextPage]);

  const items = useMemo((): FileNode[] => {
    if (inShared) return [...(shared.data ?? [])].sort((a, b) => byName(a, b, sort));
    const own = children.data?.pages.flatMap((p) => p.items) ?? [];
    const shares = shared.data ?? [];
    if (folder.id !== ROOT || shares.length === 0) return own;
    const sharedRow: FileNode = {
      id: SHARED,
      name: t("desktop.browser.shared"),
      kind: "dir",
      size: shares.reduce((sum, s) => sum + s.size, 0),
      etag: "",
      mtime: shares.reduce((last, s) => (s.sharedAt > last ? s.sharedAt : last), shares[0].sharedAt),
      permissions: ["read"],
    };
    return [sharedRow, ...own];
  }, [inShared, shared.data, children.data, folder.id, sort]);

  const ids = useMemo(() => items.map((n) => n.id), [items]);
  const marks = useQuery({
    queryKey: marksQueryKey(connId, ids),
    queryFn: () => fetchMarks(connId, ids),
    refetchInterval: MARKS_EVERY,
    placeholderData: keepPreviousData,
    retry: false,
  });
  const usage = useQuery({
    queryKey: usageQueryKey(connId),
    queryFn: () => fetchUsage(connId),
    refetchInterval: MARKS_EVERY,
    retry: false,
  });
  const notReady = marks.error instanceof ApiError && marks.error.code === "not_ready";

  const toggle = useMutation({
    mutationFn: ({ id, on }: { id: string; on: boolean }) => setChecked(connId, id, on),
    onMutate: () => setFailure(null),
    onError: (err) =>
      setFailure(err instanceof NoSpaceError ? t("desktop.noSpace", { size: formatBytes(Math.max(0, err.need - (usage.data?.free ?? 0))) }) : appErrorText(err)),
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: ["app", "marks", connId] });
      void queryClient.invalidateQueries({ queryKey: usageQueryKey(connId) });
    },
  });

  const go = (next: Folder[]) => {
    setSelected(new Set());
    setPath(next);
  };
  const up = path.length > 1 ? () => go(path.slice(0, -1)) : undefined;
  const open = (n: FileNode) => {
    if (n.kind === "dir") go([...path, { id: n.id, name: n.name }]);
  };
  const toggleSort = (key: SortKey) => {
    setSelected(new Set());
    setSort((s) => (s.key === key ? { key, order: s.order === "asc" ? "desc" : "asc" } : { key, order: key === "name" ? "asc" : "desc" }));
  };

  const check = (n: FileNode) => {
    const m: Mark | undefined = marks.data?.[n.id];
    return (
      <Checkbox
        size="xs"
        tabIndex={-1}
        aria-label={m?.check === "partial" ? t("desktop.browser.partly", { name: n.name }) : t("desktop.browser.check", { name: n.name })}
        checked={m?.check === "on"}
        indeterminate={m?.check === "partial"}
        disabled={m === undefined || toggle.isPending}
        onChange={() => toggle.mutate({ id: n.id, on: m?.check !== "on" })}
      />
    );
  };
  const badge = (n: FileNode) => {
    const sync = marks.data?.[n.id]?.sync;
    if (!sync) return null;
    const icon =
      sync === "synced" ? (
        <IconCircleCheck size={16} color="var(--mantine-color-teal-6)" aria-hidden />
      ) : sync === "pending" ? (
        <IconClock size={16} color="var(--mantine-color-blue-6)" aria-hidden />
      ) : (
        <IconAlertTriangle size={16} color="var(--mantine-color-red-6)" aria-hidden />
      );
    const label = t(`desktop.browser.sync.${sync}`);
    return (
      <Tooltip label={label} withinPortal>
        <span role="img" aria-label={label} style={{ display: "inline-flex", flexShrink: 0 }}>
          {icon}
        </span>
      </Tooltip>
    );
  };

  const listError = inShared ? shared.error : children.error;
  const loading = inShared ? shared.isPending : children.isPending;
  return (
    <Stack h="100%" gap="sm" miw={0}>
      <Group gap="xs" wrap="nowrap">
        {up && (
          <ActionIcon variant="subtle" onClick={up} aria-label={t("desktop.back")}>
            <IconArrowLeft size={18} />
          </ActionIcon>
        )}
        <Title order={4} style={{ minWidth: 0 }} lineClamp={1}>
          {folder.name}
        </Title>
        {notReady && (
          <Text size="sm" c="dimmed" ml="auto">
            {t("desktop.browser.loading")}
          </Text>
        )}
      </Group>
      {failure && (
        <Alert color="red" withCloseButton onClose={() => setFailure(null)} py="xs">
          {failure}
        </Alert>
      )}
      {items.length === 0 && listError ? (
        <ErrorState error={listError} onRetry={() => void (inShared ? shared.refetch() : children.refetch())} />
      ) : loading ? (
        <PageLoader />
      ) : items.length === 0 ? (
        <Text c="dimmed" ta="center" py="xl">
          {inShared ? t("desktop.browser.noShared") : t("files.emptyFolderTitle")}
        </Text>
      ) : (
        <FileList
          // Same URLs as in the browser: the app proxies /api to the
          // server with the connection's device token.
          thumbnail={(n) => (previewable(n) ? previewUrl(n.id, "s", n.etag) : undefined)}
          items={items}
          sort={sort}
          onSort={toggleSort}
          onOpen={open}
          selected={selected}
          onSelect={setSelected}
          // Nothing to rename or delete here: the window chooses what syncs.
          onRename={() => undefined}
          onDelete={() => undefined}
          hasMore={!inShared && hasNextPage}
          loadingMore={isFetchingNextPage}
          onLoadMore={loadMore}
          loadMoreError={isFetchNextPageError}
          onRetryMore={() => void fetchNextPage()}
          onUp={up}
          check={check}
          badge={badge}
        />
      )}
      <Group gap="xs" justify="flex-end" c="dimmed">
        {usage.data && (
          <Text size="sm" c="dimmed">
            {t("desktop.footer.selected", { size: formatBytes(usage.data.selected) })}
            {usage.data.free >= 0 && ` · ${t("desktop.footer.free", { size: formatBytes(usage.data.free) })}`}
          </Text>
        )}
      </Group>
    </Stack>
  );
}
