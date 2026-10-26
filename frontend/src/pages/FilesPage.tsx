import { ActionIcon, Anchor, Box, Breadcrumbs, Button, Group, Menu, SegmentedControl, Stack, Text, Tooltip, type ButtonProps, type MantineSize } from "@mantine/core";
import { useLocalStorage, useMediaQuery } from "@mantine/hooks";
import { notifications } from "@mantine/notifications";
import {
  IconArrowsMove,
  IconArrowsSort,
  IconCheck,
  IconCopy,
  IconDots,
  IconDownload,
  IconEdit,
  IconEye,
  IconFilePlus,
  IconFileZip,
  IconFolderOff,
  IconFolderOpen,
  IconFolderPlus,
  IconHistory,
  IconHome,
  IconLayoutGrid,
  IconLayoutSidebarRight,
  IconList,
  IconPencil,
  IconShare,
  IconTrash,
  IconUpload,
  IconX,
} from "@tabler/icons-react";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useRef, useState, type DragEvent, type MouseEvent, type ReactNode } from "react";
import { Link, useLocation, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { ApiError, errorText } from "../api";
import DetailsPanel from "../components/DetailsPanel";
import FileList, { draggedIds, isNodeDrag, type ListView } from "../components/FileList";
import MoveDialog from "../components/MoveDialog";
import NameDialog from "../components/NameDialog";
import ShareDialog from "../components/ShareDialog";
import { EmptyState, ErrorState, PageLoader } from "../components/States";
import VersionsDialog from "../components/VersionsDialog";
import Viewer from "../components/Viewer";
import { createDocument, editorMode, editorPath, editorQueryKey, fetchEditor, NEW_DOCUMENTS, openEditorTab } from "../editor";
import { followCopy } from "../jobs";
import { inlineUrl, previewable, previewUrl, viewerKind } from "../preview";
import {
  archiveUrl,
  can,
  childrenQueryKey,
  contentUrl,
  createFolder,
  download,
  fetchChildren,
  fetchNode,
  isSortKey,
  nodeQueryKey,
  renameNode,
  restoreNode,
  ROOT,
  runBatch,
  type BatchResult,
  type FileNode,
  type PathEntry,
  SORT_KEYS,
  type Sort,
  type SortKey,
} from "../files";
import { isRecord, isString } from "../guards";
import { t } from "../i18n";
import { useIsPhone } from "../media";
import { filesFromDrop, filesFromInput, useUploads } from "../uploads";
import classes from "./FilesPage.module.css";

// Sort lives in the URL so a reload or a shared link keeps it.
function useSort(): [Sort, (key: SortKey) => void, (sort: Sort) => void] {
  const [params, setParams] = useSearchParams();
  const key = params.get("sort");
  const sort: Sort = { key: isSortKey(key) ? key : "name", order: params.get("order") === "desc" ? "desc" : "asc" };
  const toggle = (next: SortKey) => {
    // Clicking the current column flips it; another column starts the
    // way people usually want it: names A→Z, newest and biggest first.
    const order = next === sort.key ? (sort.order === "asc" ? "desc" : "asc") : next === "name" ? "asc" : "desc";
    setParams({ sort: next, order }, { replace: true });
  };
  const set = (next: Sort) => setParams({ sort: next.key, order: next.order }, { replace: true });
  return [sort, toggle, set];
}

const SORT_LABELS = { name: "files.columns.name", size: "files.columns.size", mtime: "files.columns.mtime" } as const;

// How the folder is shown is the viewer's own taste, kept in this browser
// (and not in the URL, which a shared link carries to someone else).
const TILE_SIZES = { s: 120, m: 180, l: 256 } as const;
const TILE_LABELS = { s: "small", m: "medium", l: "large" } as const;
// A phone's few hundred pixels: three, two and one tiles to a line.
const PHONE_TILE_SIZES = { s: 100, m: 150, l: 300 } as const;
type TileSize = keyof typeof TILE_SIZES;

function isTileSize(v: string | undefined): v is TileSize {
  return v === "s" || v === "m" || v === "l";
}

function useViewPrefs() {
  // Read at once, not after mount: the list would show in rows first.
  const common = { getInitialValueInEffect: false, serialize: (v: string) => v };
  const [view, setView] = useLocalStorage<ListView>({ ...common, key: "cloud.files.view", defaultValue: "list", deserialize: (v) => (v === "grid" ? "grid" : "list") });
  const [tile, setTile] = useLocalStorage<TileSize>({ ...common, key: "cloud.files.tileSize", defaultValue: "m", deserialize: (v) => (isTileSize(v) ? v : "m") });
  const [panel, setPanel] = useLocalStorage<boolean>({
    key: "cloud.files.panel",
    defaultValue: false,
    getInitialValueInEffect: false,
    serialize: (v) => (v ? "1" : "0"),
    deserialize: (v) => v === "1",
  });
  return { view, setView, tile, setTile, panel, setPanel };
}

// Narrower than this, the list needs all the width there is.
const PANEL_MEDIA = "(min-width: 900px)";

// Router state of a folder opened from the keyboard: its list takes the
// focus, with from (the folder come up from) active in it.
type KeyboardArrival = { focusList: true; from?: string };

function keyboardArrival(state: unknown): KeyboardArrival | null {
  if (!isRecord(state) || state.focusList !== true) return null;
  return { focusList: true, from: isString(state.from) ? state.from : undefined };
}

export function folderPath(id: string, search: string): string {
  return id === ROOT ? `/files${search}` : `/files/${encodeURIComponent(id)}${search}`;
}

// The file manager (E9-2, E9-4): breadcrumbs, a toolbar for the selection
// and a folder's contents. Remounted per folder and order (see the key
// below), so the selection never outlives the list it was made in.
export default function FilesPage() {
  const { id = ROOT } = useParams();
  const [sort, toggleSort, setSort] = useSort();
  return <FolderView key={`${id}:${sort.key}:${sort.order}`} id={id} sort={sort} onSort={toggleSort} onSetSort={setSort} />;
}

// One thing the user can do here, shown both as a toolbar button and as a
// context menu item, so the two never disagree. items make it a submenu.
type Action = {
  key: string;
  label: string;
  icon: ReactNode;
  onClick?: () => void;
  to?: string;
  // Opens to in a new tab (the editor), leaving this one as it is.
  newTab?: boolean;
  // The toolbar's emphasis; the menu shows all alike but for the color.
  variant?: "filled" | "light";
  color?: "red";
  items?: { key: string; label: string; onClick: () => void }[];
};

function linkTarget(a: Action) {
  const to = a.to;
  if (!a.newTab || !to) return {};
  return {
    target: "_blank",
    rel: "noopener",
    // A plain click opens the tab by script, as a double click does: only
    // such a tab may close itself when the editor closes (EditorPage). A
    // modified or middle click keeps the link's own behaviour.
    onClick: (e: MouseEvent) => {
      if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      e.preventDefault();
      window.open(to, "_blank", "noopener");
    },
  };
}

function ActionButton({ action: a, size = "xs" }: { action: Action; size?: MantineSize }) {
  const common: ButtonProps = {
    size,
    variant: a.variant ?? "default",
    color: a.color,
    leftSection: a.icon,
  };
  if (a.items) {
    return (
      <Menu position="bottom-end">
        <Menu.Target>
          <Button {...common}>{a.label}</Button>
        </Menu.Target>
        <Menu.Dropdown>
          {a.items.map((i) => (
            <Menu.Item key={i.key} onClick={i.onClick}>
              {i.label}
            </Menu.Item>
          ))}
        </Menu.Dropdown>
      </Menu>
    );
  }
  if (a.to) {
    return (
      <Button {...common} component={Link} to={a.to} {...linkTarget(a)}>
        {a.label}
      </Button>
    );
  }
  return (
    <Button {...common} onClick={a.onClick}>
      {a.label}
    </Button>
  );
}

// An action as an icon alone, where a phone has no room for its label.
function ActionIconButton({ action: a }: { action: Action }) {
  const common = { size: "input-sm", variant: a.color ? "light" : "default", color: a.color, "aria-label": a.label } as const;
  return (
    <Tooltip label={a.label}>
      {a.to ? (
        <ActionIcon {...common} component={Link} to={a.to} {...linkTarget(a)}>
          {a.icon}
        </ActionIcon>
      ) : (
        <ActionIcon {...common} onClick={a.onClick}>
          {a.icon}
        </ActionIcon>
      )}
    </Tooltip>
  );
}

// On a phone's toolbar, these of the selection's actions are buttons; the
// rest are in its "more" menu.
const PHONE_KEY_ACTIONS = ["download", "share", "delete"];

function ActionItem({ action: a }: { action: Action }) {
  if (a.items) {
    return (
      <Menu.Sub>
        <Menu.Sub.Target>
          <Menu.Sub.Item leftSection={a.icon}>{a.label}</Menu.Sub.Item>
        </Menu.Sub.Target>
        <Menu.Sub.Dropdown>
          {a.items.map((i) => (
            <Menu.Item key={i.key} onClick={i.onClick}>
              {i.label}
            </Menu.Item>
          ))}
        </Menu.Sub.Dropdown>
      </Menu.Sub>
    );
  }
  if (a.to) {
    return (
      <Menu.Item component={Link} to={a.to} {...linkTarget(a)} leftSection={a.icon} color={a.color}>
        {a.label}
      </Menu.Item>
    );
  }
  return (
    <Menu.Item leftSection={a.icon} color={a.color} onClick={a.onClick}>
      {a.label}
    </Menu.Item>
  );
}

// The actions whose conditions hold.
function present(actions: (Action | false | undefined)[]): Action[] {
  return actions.filter((a): a is Action => a !== false && a !== undefined);
}

// Longer than OnlyOffice's wait before saving a closed document (5 s).
const REFETCH_AFTER_EDIT = 10_000;

type Dialog =
  | { kind: "create" }
  | { kind: "rename"; node: FileNode }
  | { kind: "move" }
  | { kind: "copy" }
  | { kind: "versions"; node: FileNode }
  | { kind: "share"; node: FileNode }
  | null;

function FolderView({ id, sort, onSort, onSetSort }: { id: string; sort: Sort; onSort: (key: SortKey) => void; onSetSort: (sort: Sort) => void }) {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const arrival = keyboardArrival(useLocation().state);
  const queryClient = useQueryClient();
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [dialog, setDialog] = useState<Dialog>(null);
  const { enqueue } = useUploads();
  const fileInput = useRef<HTMLInputElement>(null);
  const folderInput = useRef<HTMLInputElement>(null);
  // dragenter/leave fire for every child the pointer crosses; counting them
  // tells when it has really left the drop area.
  const dragDepth = useRef(0);
  const [dragging, setDragging] = useState(false);
  const [crumbTarget, setCrumbTarget] = useState<string | null>(null);
  const prefs = useViewPrefs();
  const phone = useIsPhone();
  const wide = useMediaQuery(PANEL_MEDIA, false, { getInitialValueInEffect: false });
  const panelShown = prefs.panel && wide;

  const node = useQuery({ queryKey: nodeQueryKey(id), queryFn: ({ signal }) => fetchNode(id, signal) });
  // What the online editor opens; nothing when there is none.
  const editor = useQuery({ queryKey: editorQueryKey, queryFn: ({ signal }) => fetchEditor(signal), staleTime: 10 * 60 * 1000, retry: false });
  const children = useInfiniteQuery({
    queryKey: childrenQueryKey(id, sort),
    // "" is the first page: the backend takes no cursor for it.
    queryFn: ({ pageParam, signal }) => fetchChildren(id, sort, pageParam || null, signal),
    initialPageParam: "",
    getNextPageParam: (last) => last.nextCursor ?? undefined,
  });
  const { hasNextPage, isFetchingNextPage, isFetchNextPageError, fetchNextPage } = children;
  // After a failed next page only the user's retry asks again (see FileList).
  const loadMore = useCallback(() => {
    if (hasNextPage && !isFetchingNextPage && !isFetchNextPageError) void fetchNextPage();
  }, [hasNextPage, isFetchingNextPage, isFetchNextPageError, fetchNextPage]);

  const items = children.data?.pages.flatMap((p) => p.items) ?? [];
  const selection = items.filter((n) => selected.has(n.id));
  const single = selection.length === 1 ? selection[0] : undefined;
  // Creating, uploading, renaming, moving and deleting all change this
  // folder: the server checks, the toolbar only offers what it'd allow.
  const writable = can(node.data, "write");

  // Any change can touch several folders (the one moved to, the sizes up
  // the path), so every listing and node is refetched rather than patched.
  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ["children"] });
    void queryClient.invalidateQueries({ queryKey: ["node"] });
    void queryClient.invalidateQueries({ queryKey: ["folders"] });
  };

  // A document edited in another tab changes this folder behind its back,
  // and focus refetch is off (see queryClient). Coming back refetches the
  // listing, and once more a little later: OnlyOffice writes the file only
  // seconds after its tab closed, which is when this one shows again.
  useEffect(() => {
    let later: ReturnType<typeof setTimeout> | undefined;
    const reload = () => {
      // Every order of this folder, which keeps the effect off sort.
      void queryClient.invalidateQueries({ queryKey: ["children", id] });
      void queryClient.invalidateQueries({ queryKey: nodeQueryKey(id) });
    };
    const onVisibility = () => {
      clearTimeout(later);
      if (document.visibilityState !== "visible") return;
      reload();
      later = setTimeout(reload, REFETCH_AFTER_EDIT);
    };
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      clearTimeout(later);
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [queryClient, id]);

  const search = params.toString() ? `?${params}` : "";
  // Images, PDFs and text open in the viewer, which steps through the
  // others. Documents the editor takes go there first, even text ones
  // (csv): editorMode only knows office formats, so plain text never does.
  const viewables = items.filter((n) => viewerKind(n));
  const [viewing, setViewing] = useState<number | null>(null);
  const open = (n: FileNode, via?: "keyboard") => {
    if (n.kind === "dir") navigate(folderPath(n.id, search), { state: via === "keyboard" ? { focusList: true } : undefined });
    else if (editorMode(editor.data, n.name)) openEditorTab(n.id);
    else if (viewerKind(n)) setViewing(viewables.findIndex((v) => v.id === n.id));
    else download(contentUrl(n.id));
  };
  const singleMode = single?.kind === "file" ? editorMode(editor.data, single.name) : undefined;
  const newDocuments = NEW_DOCUMENTS.filter((d) => editor.data?.[d.ext] === "edit");
  const newDocument = async (name: string) => {
    try {
      const created = await createDocument(folderId, name);
      refresh();
      // This tab, not a new one: after the request the click no longer
      // counts as the user's, and a popup blocker would stop the tab.
      navigate(editorPath(created.id));
    } catch (err) {
      notifications.show({ color: "red", message: errorText(err) });
    }
  };
  const downloadSelection = () => {
    if (single) download(single.kind === "dir" ? archiveUrl(single.id) : contentUrl(single.id));
  };

  // Tells the user how a batch went: a count of what worked, and the
  // first failure's reason when something didn't.
  // Small copies are made at once; big ones go on in the background, each
  // followed to its end.
  const copyTo = async (target: string) => {
    try {
      const results = await runBatch(selection.map((n) => ({ op: "copy", id: n.id, parentId: target })));
      const now = results.filter((r) => !r.job);
      if (now.length > 0) report(now, (count) => t("actions.copied", { count }));
      for (const r of results) {
        if (r.job) void followCopy(r.job, selection.find((n) => n.id === r.id)?.name ?? "", refresh);
      }
      setSelected(new Set());
    } catch (err) {
      notifications.show({ color: "red", message: errorText(err) });
    }
    refresh();
  };
  const report = (results: BatchResult[], done: (count: number) => string, undo?: () => void) => {
    const failed = results.filter((r) => r.error);
    const ok = results.length - failed.length;
    if (failed.length === 0) {
      notifications.show({
        message: (
          <Group justify="space-between" wrap="nowrap">
            <Text size="sm">{done(ok)}</Text>
            {undo && (
              <Button size="compact-sm" variant="subtle" onClick={undo}>
                {t("actions.undo")}
              </Button>
            )}
          </Group>
        ),
      });
      return;
    }
    notifications.show({
      color: ok > 0 ? "yellow" : "red",
      title: ok > 0 ? done(ok) : undefined,
      message: t("actions.failed", {
        count: failed.length,
        reason: errorText(new ApiError(0, failed[0].error ?? "unknown", "batch operation failed")),
      }),
    });
  };

  // ids: the selection, or the rows dragged onto a folder.
  const moveTo = async (target: string, ids = selection.map((n) => n.id)) => {
    try {
      const results = await runBatch(ids.map((nodeId) => ({ op: "move", id: nodeId, parentId: target })));
      report(results, (count) => t("actions.moved", { count }));
      setSelected(new Set());
    } catch (err) {
      notifications.show({ color: "red", message: errorText(err) });
    }
    refresh();
  };

  const remove = async () => {
    const ids = selection.map((n) => n.id);
    try {
      const results = await runBatch(ids.map((nodeId) => ({ op: "delete", id: nodeId })));
      const trashed = results.filter((r) => !r.error).map((r) => r.id);
      report(results, (count) => t("actions.trashed", { count }), async () => {
        notifications.clean();
        // Restored one by one; one coming back under a new name (its
        // place taken meanwhile) doesn't stop the others.
        const back = await Promise.allSettled(trashed.map((nodeId) => restoreNode(nodeId)));
        const failed = back.filter((r) => r.status === "rejected");
        if (failed.length > 0) notifications.show({ color: "red", message: errorText(failed[0].reason) });
        refresh();
      });
      setSelected(new Set());
    } catch (err) {
      notifications.show({ color: "red", message: errorText(err) });
    }
    refresh();
  };

  // Files only: dragging text or a link in from elsewhere isn't an upload.
  const isFileDrag = (e: DragEvent) => Array.from(e.dataTransfer.types).includes("Files");
  const dragEnter = (e: DragEvent) => {
    if (!isFileDrag(e) || !writable) return;
    e.preventDefault();
    dragDepth.current++;
    setDragging(true);
  };
  const dragLeave = (e: DragEvent) => {
    if (!isFileDrag(e)) return;
    dragDepth.current = Math.max(0, dragDepth.current - 1);
    if (dragDepth.current === 0) setDragging(false);
  };
  const drop = async (e: DragEvent) => {
    if (!isFileDrag(e) || !writable) return;
    e.preventDefault();
    dragDepth.current = 0;
    setDragging(false);
    // The item list is only readable during the event: walked right away.
    const files = await filesFromDrop(e.dataTransfer.items);
    if (files.length > 0) enqueue(files, node.data?.id ?? id);
  };
  const picked = (input: HTMLInputElement | null) => {
    if (!input?.files || input.files.length === 0) return;
    enqueue(filesFromInput(input.files), node.data?.id ?? id);
    // So picking the same file again fires change again.
    input.value = "";
  };

  // Only a folder that never loaded is replaced by the error: a failed
  // next page or background refetch keeps what's on screen, selection too.
  const error = (node.data === undefined ? node.error : null) ?? (children.data === undefined ? children.error : null);
  if (error) {
    const toRoot = (
      <Button component={Link} to="/files" variant="light">
        {t("files.toRoot")}
      </Button>
    );
    if (error instanceof ApiError && error.code === "not_found") {
      return (
        <EmptyState
          icon={<IconFolderOff size={36} stroke={1.5} />}
          color="gray"
          title={t("files.folderNotFound")}
          description={t("files.folderNotFoundDescription")}
          action={toRoot}
        />
      );
    }
    const retry = () => {
      if (node.isError) void node.refetch();
      if (children.isError) void children.refetch();
    };
    return <ErrorState error={error} onRetry={retry} action={id === ROOT ? undefined : toRoot} />;
  }

  const path = node.data?.path ?? [];
  const folderId = node.data?.id ?? id;
  // Someone else's folder: the path starts at what they shared, not at a
  // root of the user's own.
  const sharedTree = path.length > 0 && path[0].name !== "";
  // Up a level from the keyboard, the list keeping the focus there; the
  // top of the path has nowhere to go.
  const parent = path.length >= 2 ? path[path.length - 2] : undefined;
  const up = parent && (() => navigate(folderPath(parent.name === "" ? ROOT : parent.id, search), { state: { focusList: true, from: folderId } }));

  // Rows dragged onto a folder up the path move there. The path carries no
  // rights, so any folder up it is offered; the server refuses what it must.
  // Moving out changes this folder, hence writable.
  const crumbDrop = (target: string) =>
    writable
      ? {
          "data-drop-target": crumbTarget === target || undefined,
          onDragOver: (e: DragEvent) => {
            if (!isNodeDrag(e.dataTransfer)) return;
            e.preventDefault();
            e.dataTransfer.dropEffect = "move";
            setCrumbTarget(target);
          },
          onDragLeave: () => setCrumbTarget(null),
          onDrop: (e: DragEvent) => {
            const ids = draggedIds(e.dataTransfer);
            setCrumbTarget(null);
            if (!ids) return;
            e.preventDefault();
            void moveTo(target, ids);
          },
        }
      : {};

  // What the toolbar and the context menu offer: for the selection when
  // there is one, otherwise for the folder.
  const actions: Action[] =
    selection.length > 0
      ? present([
          single &&
            singleMode && {
              key: "editor",
              label: singleMode === "edit" && can(single, "write") ? t("editor.open") : t("editor.view"),
              icon: singleMode === "edit" ? <IconEdit size={16} /> : <IconEye size={16} />,
              to: editorPath(single.id),
              newTab: true,
              variant: "filled",
            },
          single && {
            key: "download",
            label: t("actions.download"),
            icon: <IconDownload size={16} />,
            onClick: downloadSelection,
          },
          single?.kind === "file" && {
            key: "versions",
            label: t("versions.button"),
            icon: <IconHistory size={16} />,
            onClick: () => setDialog({ kind: "versions", node: single }),
          },
          single &&
            can(single, "reshare") && {
              key: "share",
              label: t("share.button"),
              icon: <IconShare size={16} />,
              onClick: () => setDialog({ kind: "share", node: single }),
            },
          single &&
            writable && {
              key: "rename",
              label: t("actions.rename"),
              icon: <IconPencil size={16} />,
              onClick: () => setDialog({ kind: "rename", node: single }),
            },
          {
            key: "copy",
            label: t("actions.copy"),
            icon: <IconCopy size={16} />,
            onClick: () => setDialog({ kind: "copy" }),
          },
          writable && {
            key: "move",
            label: t("actions.move"),
            icon: <IconArrowsMove size={16} />,
            onClick: () => setDialog({ kind: "move" }),
          },
          writable && {
            key: "delete",
            label: t("actions.delete"),
            icon: <IconTrash size={16} />,
            onClick: () => void remove(),
            variant: "light",
            color: "red",
          },
        ])
      : present([
          items.length > 0 && {
            key: "downloadFolder",
            label: t("actions.downloadFolder"),
            icon: <IconFileZip size={16} />,
            onClick: () => download(archiveUrl(folderId)),
          },
          writable && {
            key: "newFolder",
            label: t("actions.newFolder"),
            icon: <IconFolderPlus size={16} />,
            onClick: () => setDialog({ kind: "create" }),
          },
          writable &&
            newDocuments.length > 0 && {
              key: "newDocument",
              label: t("editor.create"),
              icon: <IconFilePlus size={16} />,
              items: newDocuments.map((d) => ({
                key: d.ext,
                label: t(d.label),
                onClick: () => void newDocument(`${t(d.label)}.${d.ext}`),
              })),
            },
          writable && {
            key: "upload",
            label: t("upload.button"),
            icon: <IconUpload size={16} />,
            variant: "filled",
            items: [
              {
                key: "files",
                label: t("upload.files"),
                onClick: () => fileInput.current?.click(),
              },
              {
                key: "folder",
                label: t("upload.folder"),
                onClick: () => folderInput.current?.click(),
              },
            ],
          },
        ]);

  const emptyState = (
    <EmptyState
      icon={<IconFolderOpen size={36} stroke={1.5} />}
      title={id === ROOT ? t("files.emptyTitle") : t("files.emptyFolderTitle")}
      description={writable ? t("files.emptyDescription") : undefined}
    />
  );

  // A phone keeps the path to one line: the top, "…" for the folders
  // between (a menu of them) and this one.
  const hiddenPath = phone && path.length > 2 ? path.slice(1, -1) : [];
  const shownPath = hiddenPath.length > 0 ? [path[0], path[path.length - 1]] : path;
  const rootIcon = (p: PathEntry) => phone && p.name === "";
  const crumbs = (
    <Breadcrumbs separatorMargin={4} style={{ minWidth: 0 }} className={phone ? classes.crumbsLine : undefined}>
      {sharedTree && (
        <Anchor component={Link} to="/shared" truncate="end">
          {t("shared.crumb")}
        </Anchor>
      )}
      {shownPath.map((p, i) => [
        i === 1 && hiddenPath.length > 0 && (
          <Menu key="hidden" position="bottom-start">
            <Menu.Target>
              <Anchor component="button" type="button" aria-label={t("files.crumbsHidden")}>
                …
              </Anchor>
            </Menu.Target>
            <Menu.Dropdown>
              {hiddenPath.map((h) => (
                <Menu.Item key={h.id} component={Link} to={folderPath(h.id, search)}>
                  {h.name}
                </Menu.Item>
              ))}
            </Menu.Dropdown>
          </Menu>
        ),
        i === shownPath.length - 1 ? (
          <Text key={p.id} fw={600} truncate="end">
            {p.name || t("files.root")}
          </Text>
        ) : (
          <Anchor
            key={p.id}
            component={Link}
            to={folderPath(p.name === "" ? ROOT : p.id, search)}
            truncate="end"
            className={classes.crumb}
            {...crumbDrop(p.id)}
            // A phone's line is too short for the root's name: its icon
            // leaves the room to the folder shown.
            {...(rootIcon(p) ? { "aria-label": t("files.root"), title: t("files.root"), "data-icon": true } : {})}
          >
            {rootIcon(p) ? <IconHome size={18} aria-hidden /> : p.name || t("files.root")}
          </Anchor>
        ),
      ])}
    </Breadcrumbs>
  );
  const sortItems = (
    <>
      <Menu.Label>{t("files.sort.label")}</Menu.Label>
      {SORT_KEYS.map((key) => (
        <Menu.Item
          key={key}
          leftSection={<IconCheck size={14} visibility={sort.key === key ? "visible" : "hidden"} />}
          // As a column header click starts it: names A→Z, the rest biggest and newest first.
          onClick={() => onSetSort({ key, order: key === sort.key ? sort.order : key === "name" ? "asc" : "desc" })}
        >
          {t(SORT_LABELS[key])}
        </Menu.Item>
      ))}
      <Menu.Divider />
      {(["asc", "desc"] as const).map((order) => (
        <Menu.Item
          key={order}
          leftSection={<IconCheck size={14} visibility={sort.order === order ? "visible" : "hidden"} />}
          onClick={() => onSetSort({ key: sort.key, order })}
        >
          {t(`files.sort.${order}`)}
        </Menu.Item>
      ))}
    </>
  );

  // The phone's toolbar: the selection's key actions as icons, or Upload,
  // and a "more" menu with the rest; without a selection, also the view.
  const phoneKeys = actions.filter((a) => PHONE_KEY_ACTIONS.includes(a.key));
  const upload = selection.length === 0 ? actions.find((a) => a.key === "upload") : undefined;
  const phoneMore = (rest: Action[], withView: boolean) =>
    (rest.length > 0 || withView) && (
      <Menu position="bottom-end">
        <Menu.Target>
          <ActionIcon size="input-sm" variant="default" aria-label={t("files.more")}>
            <IconDots size={18} />
          </ActionIcon>
        </Menu.Target>
        <Menu.Dropdown>
          {rest.map((a) => (
            <ActionItem key={a.key} action={a} />
          ))}
          {withView && (
            <>
              {rest.length > 0 && <Menu.Divider />}
              {(["list", "grid"] as const).map((v) => (
                <Menu.Item
                  key={v}
                  leftSection={v === "list" ? <IconList size={16} /> : <IconLayoutGrid size={16} />}
                  rightSection={prefs.view === v && <IconCheck size={14} />}
                  aria-pressed={prefs.view === v}
                  onClick={() => prefs.setView(v)}
                >
                  {t(`files.view.${v}`)}
                </Menu.Item>
              ))}
              {prefs.view === "grid" && (
                <>
                  <Menu.Label>{t("files.view.tileSize")}</Menu.Label>
                  {(["s", "m", "l"] as const).map((size) => (
                    <Menu.Item
                      key={size}
                      leftSection={<IconCheck size={14} visibility={prefs.tile === size ? "visible" : "hidden"} />}
                      onClick={() => prefs.setTile(size)}
                    >
                      {t(`files.view.${TILE_LABELS[size]}`)}
                    </Menu.Item>
                  ))}
                </>
              )}
              <Menu.Divider />
              {sortItems}
            </>
          )}
        </Menu.Dropdown>
      </Menu>
    );

  return (
    <Stack
      h="calc(100dvh - 56px - 2 * var(--mantine-spacing-md))"
      gap="sm"
      pos="relative"
      onDragEnter={dragEnter}
      onDragOver={(e) => isFileDrag(e) && e.preventDefault()}
      onDragLeave={dragLeave}
      onDrop={(e) => void drop(e)}
    >
      {dragging && (
        <Box className={classes.dropOverlay} aria-hidden>
          <Group gap="xs">
            <IconUpload size={24} />
            <Text fw={600}>{t("upload.drop")}</Text>
          </Group>
        </Box>
      )}
      <input ref={fileInput} type="file" multiple hidden data-testid="upload-files" onChange={(e) => picked(e.currentTarget)} />
      <input
        ref={folderInput}
        type="file"
        multiple
        hidden
        data-testid="upload-folder"
        // React has no typed prop for it; the attribute is what browsers read.
        {...{ webkitdirectory: "" }}
        onChange={(e) => picked(e.currentTarget)}
      />
      {phone ? (
        // One line, whatever there is: the path or the selection's count,
        // the main actions as buttons and the rest in "more".
        <Group gap="xs" wrap="nowrap" justify="space-between">
          {selection.length > 0 ? (
            <>
              <Group gap={4} wrap="nowrap" miw={0}>
                <ActionIcon size="input-sm" variant="subtle" color="gray" aria-label={t("files.clearSelection")} onClick={() => setSelected(new Set())}>
                  <IconX size={18} />
                </ActionIcon>
                <Text size="sm" fw={600} truncate="end" aria-live="polite">
                  {t("files.selected", { count: selection.length })}
                </Text>
              </Group>
              <Group gap="xs" wrap="nowrap">
                {phoneKeys.map((a) => (
                  <ActionIconButton key={a.key} action={a} />
                ))}
                {phoneMore(actions.filter((a) => !phoneKeys.includes(a)), false)}
              </Group>
            </>
          ) : (
            <>
              {crumbs}
              <Group gap="xs" wrap="nowrap">
                {upload && <ActionButton action={upload} size="sm" />}
                {phoneMore(
                  actions.filter((a) => a !== upload),
                  true,
                )}
              </Group>
            </>
          )}
        </Group>
      ) : (
        <Group justify="space-between" gap="sm">
          {crumbs}
          <Group gap="xs">
            {/* In the toolbar's row, which is always there: a line of its own
                above the list would push the rows down under the pointer the
                moment a click, right-click or drag selects one. */}
            {selection.length > 0 && (
              <Text size="sm" c="dimmed" aria-live="polite">
                {t("files.selected", { count: selection.length })}
              </Text>
            )}
            {selection.length > 0 ? (
              // The selection's actions are many and change with what is
              // selected: in one menu the toolbar keeps its width.
              <Menu position="bottom-end">
                <Menu.Target>
                  <Button size="xs" variant="default" leftSection={<IconDots size={16} />}>
                    {t("files.actions")}
                  </Button>
                </Menu.Target>
                <Menu.Dropdown>
                  {actions.map((a) => (
                    <ActionItem key={a.key} action={a} />
                  ))}
                </Menu.Dropdown>
              </Menu>
            ) : (
              actions.map((a) => <ActionButton key={a.key} action={a} />)
            )}
            {/* Grid has no column headers to sort by. */}
            {prefs.view === "grid" && (
              <Menu position="bottom-end">
                <Tooltip label={t("files.sort.label")}>
                  <Menu.Target>
                    <ActionIcon size="md" variant="default" aria-label={t("files.sort.label")}>
                      <IconArrowsSort size={16} />
                    </ActionIcon>
                  </Menu.Target>
                </Tooltip>
                <Menu.Dropdown>
                  {sortItems}
                </Menu.Dropdown>
              </Menu>
            )}
            {prefs.view === "grid" && (
              <SegmentedControl
                size="xs"
                aria-label={t("files.view.tileSize")}
                value={prefs.tile}
                onChange={(v) => {
                  if (isTileSize(v)) prefs.setTile(v);
                }}
                data={[
                  { value: "s", label: t("files.view.small") },
                  { value: "m", label: t("files.view.medium") },
                  { value: "l", label: t("files.view.large") },
                ]}
              />
            )}
            <ActionIcon.Group>
              {(["list", "grid"] as const).map((v) => (
                <Tooltip key={v} label={t(`files.view.${v}`)}>
                  <ActionIcon
                    size="md"
                    variant={prefs.view === v ? "filled" : "default"}
                    aria-label={t(`files.view.${v}`)}
                    aria-pressed={prefs.view === v}
                    onClick={() => prefs.setView(v)}
                  >
                    {v === "list" ? <IconList size={16} /> : <IconLayoutGrid size={16} />}
                  </ActionIcon>
                </Tooltip>
              ))}
            </ActionIcon.Group>
            {wide && (
              <Tooltip label={t("files.panel.toggle")}>
                <ActionIcon
                  size="md"
                  variant={prefs.panel ? "filled" : "default"}
                  aria-label={t("files.panel.toggle")}
                  aria-pressed={prefs.panel}
                  onClick={() => prefs.setPanel(!prefs.panel)}
                >
                  <IconLayoutSidebarRight size={16} />
                </ActionIcon>
              </Tooltip>
            )}
          </Group>
        </Group>
      )}
      {/* The context menu at the cursor: right-click on a row (FileList has
          selected it by then) or below the rows offers the toolbar's actions. */}
      <Box className={classes.body}>
        <Menu position="bottom-start" offset={2}>
          <Menu.ContextMenu disabled={actions.length === 0}>
            <Box className={classes.listArea}>
              {children.isPending ? (
                <PageLoader />
              ) : items.length === 0 && !up ? (
                emptyState
              ) : (
                <FileList
                  empty={emptyState}
                  thumbnail={(n) => (previewable(n) ? previewUrl(n.id, "s", n.etag) : undefined)}
                  items={items}
                  sort={sort}
                  onSort={onSort}
                  onOpen={open}
                  selected={selected}
                  onSelect={setSelected}
                  onRename={(n) => {
                    if (writable) setDialog({ kind: "rename", node: n });
                  }}
                  onDelete={() => {
                    if (writable) void remove();
                  }}
                  hasMore={hasNextPage}
                  loadingMore={isFetchingNextPage}
                  onLoadMore={loadMore}
                  loadMoreError={isFetchNextPageError}
                  onRetryMore={() => void fetchNextPage()}
                  withContextMenu
                  onMoveInto={writable ? (ids, folder) => void moveTo(folder.id, ids) : undefined}
                  canMoveInto={(f) => can(f, "write")}
                  view={prefs.view}
                  tileSize={(phone ? PHONE_TILE_SIZES : TILE_SIZES)[prefs.tile]}
                  onUp={up}
                  autoFocus={arrival !== null}
                />
              )}
            </Box>
          </Menu.ContextMenu>
          <Menu.Dropdown aria-label={t("files.menu")}>
            {actions.map((a) => (
              <ActionItem key={a.key} action={a} />
            ))}
          </Menu.Dropdown>
        </Menu>
        {panelShown && (
          <DetailsPanel
            selection={selection}
            onOpen={open}
            actions={actions
              .filter((a) => a.key === "download" || a.key === "versions")
              .map((a) => (
                <ActionButton key={a.key} action={a} />
              ))}
          />
        )}
      </Box>
      {viewing !== null && (
        <Viewer
          items={viewables}
          index={viewing}
          onIndex={setViewing}
          onClose={() => setViewing(null)}
          imageUrl={(n) => previewUrl(n.id, "l", n.etag)}
          pdfUrl={(n) => inlineUrl(n.id)}
          textUrl={(n) => contentUrl(n.id)}
          onDownload={(n) => download(contentUrl(n.id))}
        />
      )}
      <NameDialog
        opened={dialog?.kind === "create"}
        title={t("actions.newFolder")}
        submitLabel={t("actions.create")}
        onClose={() => setDialog(null)}
        onSubmit={async (name) => {
          await createFolder(folderId, name);
          refresh();
        }}
      />
      <NameDialog
        opened={dialog?.kind === "rename"}
        title={t("actions.renameTitle")}
        submitLabel={t("actions.rename")}
        initial={dialog?.kind === "rename" ? dialog.node.name : ""}
        onClose={() => setDialog(null)}
        onSubmit={async (name) => {
          if (dialog?.kind !== "rename") return;
          await renameNode(dialog.node.id, name);
          refresh();
        }}
      />
      <VersionsDialog file={dialog?.kind === "versions" ? dialog.node : null} onClose={() => setDialog(null)} />
      <ShareDialog node={dialog?.kind === "share" ? dialog.node : null} onClose={() => setDialog(null)} />
      <MoveDialog
        opened={dialog?.kind === "move"}
        from={folderId}
        movingIds={new Set(selection.map((n) => n.id))}
        onClose={() => setDialog(null)}
        onMove={moveTo}
      />
      <MoveDialog
        copy
        opened={dialog?.kind === "copy"}
        from={folderId}
        movingIds={new Set(selection.map((n) => n.id))}
        onClose={() => setDialog(null)}
        onMove={copyTo}
      />
    </Stack>
  );
}
