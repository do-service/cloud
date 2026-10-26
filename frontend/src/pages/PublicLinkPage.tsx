import { Alert, Anchor, Breadcrumbs, Button, Center, Group, Paper, PasswordInput, Progress, Stack, Text, ThemeIcon, Title } from "@mantine/core";
import { notifications } from "@mantine/notifications";
import { IconDownload, IconEye, IconFile, IconFileZip, IconLinkOff, IconLock, IconUpload } from "@tabler/icons-react";
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useRef, useState, type DragEvent, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { Upload } from "tus-js-client";
import { ApiError, errorText } from "../api";
import FileList from "../components/FileList";
import { EmptyState, ErrorState, PageLoader } from "../components/States";
import Viewer from "../components/Viewer";
import { download, type FileNode, type Sort, type SortKey } from "../files";
import { formatBytes, t } from "../i18n";
import {
  fetchLinkChildren,
  fetchLinkInfo,
  fetchLinkNode,
  linkArchiveUrl,
  linkContentUrl,
  linkInfoQueryKey,
  linkInlineUrl,
  linkPreviewUrl,
  linkUploadEndpoint,
  unlockLink,
  viewable,
  type LinkInfo,
} from "../publicLink";
import { mediaKind, previewable } from "../preview";
import { uploadError } from "../uploads";

// A public link (R10, E9-8): no account needed. What it shows depends on
// the link: a password first if it has one, then the folder or file to
// view or download, or a drop zone for an upload link.
export default function PublicLinkPage() {
  const { token = "" } = useParams();
  const info = useQuery({ queryKey: linkInfoQueryKey(token), queryFn: ({ signal }) => fetchLinkInfo(token, signal), retry: false });

  if (info.isPending) return <PageLoader />;
  if (info.data === undefined) {
    // Gone is final for this address: nothing to retry.
    if (info.error instanceof ApiError && info.error.code === "not_found") {
      return <EmptyState icon={<IconLinkOff size={36} stroke={1.5} />} color="gray" title={t("public.goneTitle")} description={t("public.goneDescription")} />;
    }
    return <ErrorState error={info.error} title={t("public.errorTitle")} onRetry={() => void info.refetch()} />;
  }
  const link = info.data;
  if (link.needsPassword && !link.unlocked) return <PasswordGate token={token} link={link} />;
  if (link.mode === "upload") return <DropZone token={token} link={link} />;
  if (link.kind === "file") return <SharedFile token={token} link={link} />;
  return <SharedFolder token={token} link={link} />;
}

function PasswordGate({ token, link }: { token: string; link: LinkInfo }) {
  const queryClient = useQueryClient();
  const [password, setPassword] = useState("");
  const unlock = useMutation({
    mutationFn: () => unlockLink(token, password),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: linkInfoQueryKey(token) }),
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    unlock.mutate();
  };
  return (
    <Center py="xl">
      <Paper withBorder shadow="sm" p="xl" w="100%" maw={400}>
        <form onSubmit={submit}>
          <Stack>
            <Group gap="xs" justify="center">
              <IconLock size={20} aria-hidden />
              <Title order={3}>{link.name}</Title>
            </Group>
            <Text size="sm" c="dimmed" ta="center">
              {t("public.passwordHint")}
            </Text>
            <PasswordInput label={t("login.password")} value={password} onChange={(e) => setPassword(e.currentTarget.value)} required autoFocus />
            {unlock.isError && <Alert color="red">{errorText(unlock.error)}</Alert>}
            <Button type="submit" loading={unlock.isPending}>
              {t("public.open")}
            </Button>
          </Stack>
        </form>
      </Paper>
    </Center>
  );
}

// open shows images and PDFs in the viewer (onView); other files a view
// link shows in a new tab, a download link downloads. Text stays out of the
// viewer here: a view link serves only plain text, not code or markdown.
function useOpenFile(token: string, link: LinkInfo, onView: (n: FileNode) => void) {
  return (n: FileNode) => {
    if (mediaKind(n)) {
      onView(n);
    } else if (link.mode === "download") {
      download(linkContentUrl(token, n.id));
    } else if (viewable(n)) {
      window.open(linkContentUrl(token, n.id), "_blank", "noopener");
    } else {
      notifications.show({ message: t("public.notViewable") });
    }
  };
}

// LinkViewer is the viewer over a link's files.
function LinkViewer({ token, link, items, index, onIndex, onClose }: {
  token: string;
  link: LinkInfo;
  items: FileNode[];
  index: number;
  onIndex: (i: number) => void;
  onClose: () => void;
}) {
  return (
    <Viewer
      items={items}
      index={index}
      onIndex={onIndex}
      onClose={onClose}
      imageUrl={(n) => linkPreviewUrl(token, n.id, "l", n.etag)}
      pdfUrl={(n) => linkInlineUrl(token, n.id)}
      onDownload={link.mode === "download" ? (n) => download(linkContentUrl(token, n.id)) : undefined}
    />
  );
}

function SharedFile({ token, link }: { token: string; link: LinkInfo }) {
  const file = useQuery({ queryKey: ["link", token, "node", link.rootId], queryFn: ({ signal }) => fetchLinkNode(token, link.rootId, signal) });
  const [viewing, setViewing] = useState(false);
  const open = useOpenFile(token, link, () => setViewing(true));
  return (
    <Center py="xl">
      {viewing && file.data && <LinkViewer token={token} link={link} items={[file.data]} index={0} onIndex={() => undefined} onClose={() => setViewing(false)} />}
      <Paper withBorder shadow="sm" p="xl" w="100%" maw={460}>
        <Stack align="center" ta="center">
          {file.data && previewable(file.data) ? (
            <img src={linkPreviewUrl(token, file.data.id, "s", file.data.etag)} alt="" style={{ maxWidth: 256, maxHeight: 256, borderRadius: 8 }} />
          ) : (
            <ThemeIcon size={64} radius="xl" variant="light">
              <IconFile size={36} stroke={1.5} />
            </ThemeIcon>
          )}
          <Title order={3} style={{ wordBreak: "break-word" }}>
            {link.name}
          </Title>
          {file.data && <Text c="dimmed">{formatBytes(file.data.size)}</Text>}
          {file.isPending && <PageLoader compact />}
          {file.data === undefined && file.isError && <ErrorState compact error={file.error} onRetry={() => void file.refetch()} />}
          {file.data && (
            <Button
              leftSection={link.mode === "download" && !mediaKind(file.data) ? <IconDownload size={16} /> : <IconEye size={16} />}
              onClick={() => file.data && open(file.data)}
            >
              {link.mode === "download" && !mediaKind(file.data) ? t("actions.download") : t("public.view")}
            </Button>
          )}
        </Stack>
      </Paper>
    </Center>
  );
}

function SharedFolder({ token, link }: { token: string; link: LinkInfo }) {
  const { id = link.rootId } = useParams();
  const navigate = useNavigate();
  const [sort, setSort] = useState<Sort>({ key: "name", order: "asc" });
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [viewing, setViewing] = useState<number | null>(null);

  const node = useQuery({ queryKey: ["link", token, "node", id], queryFn: ({ signal }) => fetchLinkNode(token, id, signal) });
  const children = useInfiniteQuery({
    queryKey: ["link", token, "children", id, sort.key, sort.order],
    queryFn: ({ pageParam, signal }) => fetchLinkChildren(token, id, sort, pageParam || null, signal),
    initialPageParam: "",
    getNextPageParam: (last) => last.nextCursor ?? undefined,
  });
  const { hasNextPage, isFetchingNextPage, isFetchNextPageError, fetchNextPage } = children;
  // After a failed next page only the user's retry asks again (see FileList).
  const loadMore = useCallback(() => {
    if (hasNextPage && !isFetchingNextPage && !isFetchNextPageError) void fetchNextPage();
  }, [hasNextPage, isFetchingNextPage, isFetchNextPageError, fetchNextPage]);

  const items = children.data?.pages.flatMap((p) => p.items) ?? [];
  const viewables = items.filter((n) => mediaKind(n));
  const open = useOpenFile(token, link, (n) => setViewing(viewables.findIndex((v) => v.id === n.id)));
  const selection = items.filter((n) => selected.has(n.id));
  const single = selection.length === 1 ? selection[0] : undefined;
  const toggleSort = (key: SortKey) => {
    setSelected(new Set());
    setSort((s) => (s.key === key ? { key, order: s.order === "asc" ? "desc" : "asc" } : { key, order: key === "name" ? "asc" : "desc" }));
  };
  const openNode = (n: FileNode) => {
    if (n.kind === "dir") {
      setSelected(new Set());
      navigate(`/s/${token}/${n.id}`);
    } else open(n);
  };

  // Only a folder that never loaded is replaced by the error, as in the
  // file manager.
  const error = (node.data === undefined ? node.error : null) ?? (children.data === undefined ? children.error : null);
  if (error) {
    const retry = () => {
      if (node.isError) void node.refetch();
      if (children.isError) void children.refetch();
    };
    return <ErrorState error={error} onRetry={retry} />;
  }
  const path = node.data?.path ?? [];
  return (
    <Stack h="calc(100dvh - 56px - 2 * var(--mantine-spacing-md))" gap="sm">
      <Group justify="space-between" gap="sm">
        <Breadcrumbs separatorMargin={4} style={{ minWidth: 0 }}>
          {path.map((p, i) =>
            i === path.length - 1 ? (
              <Text key={p.id} fw={600} truncate="end">
                {p.name}
              </Text>
            ) : (
              <Anchor key={p.id} component={Link} to={i === 0 ? `/s/${token}` : `/s/${token}/${p.id}`} truncate="end">
                {p.name}
              </Anchor>
            ),
          )}
        </Breadcrumbs>
        <Group gap="xs">
          {/* Next to the buttons, as in the file manager: a line above the
              list would shift the rows when one is selected. */}
          {selection.length > 0 && (
            <Text size="sm" c="dimmed" aria-live="polite">
              {t("files.selected", { count: selection.length })}
            </Text>
          )}
          {link.mode === "download" && single?.kind === "file" && (
            <Button variant="default" size="xs" leftSection={<IconDownload size={16} />} onClick={() => download(linkContentUrl(token, single.id))}>
              {t("actions.download")}
            </Button>
          )}
          {link.mode === "download" && (
            <Button size="xs" leftSection={<IconFileZip size={16} />} onClick={() => download(linkArchiveUrl(token, single?.kind === "dir" ? single.id : id))}>
              {single?.kind === "dir" ? t("public.downloadFolder") : t("public.downloadAll")}
            </Button>
          )}
        </Group>
      </Group>
      {children.isPending ? (
        <PageLoader />
      ) : items.length === 0 ? (
        <Text c="dimmed" ta="center" py="xl">
          {t("files.emptyFolderTitle")}
        </Text>
      ) : (
        <FileList
          thumbnail={(n) => (previewable(n) ? linkPreviewUrl(token, n.id, "s", n.etag) : undefined)}
          items={items}
          sort={sort}
          onSort={toggleSort}
          onOpen={openNode}
          selected={selected}
          onSelect={setSelected}
          // Nothing to rename or delete through a link.
          onRename={() => undefined}
          onDelete={() => undefined}
          hasMore={hasNextPage}
          loadingMore={isFetchingNextPage}
          onLoadMore={loadMore}
          loadMoreError={isFetchNextPageError}
          onRetryMore={() => void fetchNextPage()}
        />
      )}
      {viewing !== null && (
        <LinkViewer token={token} link={link} items={viewables} index={viewing} onIndex={setViewing} onClose={() => setViewing(null)} />
      )}
    </Stack>
  );
}

type Dropped = { key: number; name: string; size: number; sent: number; status: "uploading" | "done" | "error"; error?: unknown };

// A file drop: pick or drop files, watch them go. Nothing in the folder is
// shown, and a name already there is kept beside, never replaced.
function DropZone({ token, link }: { token: string; link: LinkInfo }) {
  const [files, setFiles] = useState<Dropped[]>([]);
  const [dragging, setDragging] = useState(false);
  const input = useRef<HTMLInputElement>(null);
  const nextKey = useRef(1);

  const update = (key: number, patch: Partial<Dropped>) => setFiles((all) => all.map((f) => (f.key === key ? { ...f, ...patch } : f)));
  const send = (list: File[]) => {
    for (const file of list) {
      const key = nextKey.current++;
      setFiles((all) => [...all, { key, name: file.name, size: file.size, sent: 0, status: "uploading" }]);
      new Upload(file, {
        endpoint: linkUploadEndpoint(token),
        chunkSize: 64 * 1024 * 1024,
        retryDelays: [0, 1000, 3000, 5000, 10_000],
        metadata: { filename: file.name },
        onProgress: (sent) => update(key, { sent }),
        onSuccess: () => update(key, { status: "done", sent: file.size }),
        onError: (err) => update(key, { status: "error", error: uploadError(err) }),
      }).start();
    }
  };
  const drop = (e: DragEvent) => {
    e.preventDefault();
    setDragging(false);
    send(Array.from(e.dataTransfer.files));
  };

  return (
    <Center py="xl">
      <Stack w="100%" maw={560}>
        <Title order={3} ta="center">
          {t("public.dropTitle", { name: link.name })}
        </Title>
        <Text c="dimmed" ta="center" size="sm">
          {t("public.dropHint")}
        </Text>
        <Paper
          withBorder
          p="xl"
          style={{ borderStyle: "dashed", borderColor: dragging ? "var(--mantine-primary-color-filled)" : undefined }}
          onDragOver={(e) => {
            e.preventDefault();
            setDragging(true);
          }}
          onDragLeave={() => setDragging(false)}
          onDrop={drop}
          data-testid="drop-zone"
        >
          <Stack align="center">
            <IconUpload size={36} stroke={1.5} aria-hidden />
            <Text size="sm">{t("public.dropHere")}</Text>
            <Button onClick={() => input.current?.click()}>{t("public.chooseFiles")}</Button>
            <input
              ref={input}
              type="file"
              multiple
              hidden
              data-testid="drop-input"
              onChange={(e) => {
                send(Array.from(e.currentTarget.files ?? []));
                e.currentTarget.value = "";
              }}
            />
          </Stack>
        </Paper>
        {files.map((f) => (
          <Stack key={f.key} gap={4}>
            <Group justify="space-between" wrap="nowrap">
              <Text size="sm" truncate="end">
                {f.name}
              </Text>
              <Text size="xs" c={f.status === "error" ? "red" : "dimmed"}>
                {f.status === "done"
                  ? t("upload.done")
                  : f.status === "error"
                    ? errorText(f.error)
                    : `${formatBytes(f.sent)} / ${formatBytes(f.size)}`}
              </Text>
            </Group>
            {f.status === "uploading" && <Progress value={f.size > 0 ? (f.sent / f.size) * 100 : 100} size="xs" />}
          </Stack>
        ))}
      </Stack>
    </Center>
  );
}
