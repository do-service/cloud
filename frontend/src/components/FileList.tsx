import { ActionIcon, Box, Button, Checkbox, Group, Loader, Text, UnstyledButton } from "@mantine/core";
import {
  IconChevronDown,
  IconChevronUp,
  IconDots,
  IconFile,
  IconFileText,
  IconFileTypePdf,
  IconFileZip,
  IconFolderFilled,
  IconMovie,
  IconMusic,
  IconPhoto,
} from "@tabler/icons-react";
import { useVirtualizer } from "@tanstack/react-virtual";
import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type DragEvent,
  type KeyboardEvent,
  type MouseEvent,
  type PointerEvent,
  type ReactNode,
  type TouchEvent,
} from "react";
import type { FileNode, Sort, SortKey } from "../files";
import { isArrayOf, isString } from "../guards";
import { formatBytes, formatDate, t, type MessageKey } from "../i18n";
import { useCoarsePointer, useIsPhone } from "../media";
import { extension } from "../preview";
import classes from "./FileList.module.css";

export const ROW_HEIGHT = 44;
// On a phone a row has a second line (size and date) and room for a finger.
export const PHONE_ROW_HEIGHT = 56;

// A touch held this long, moving less than LONG_PRESS_SLOP px, is a long
// press: it selects instead of opening.
const LONG_PRESS_MS = 500;
const LONG_PRESS_SLOP = 10;

export type ListView = "list" | "grid";

// A grid row is a tile and the gap below it; the tile's picture gets what
// its name (two lines) and padding leave (see .tile).
const TILE_EXTRA = 40;

// How close to the end of what's loaded the next page is requested: a
// screenful ahead, so fast scrolling rarely hits the loading row.
const PREFETCH_ROWS = 40;

// Rows dragged inside the app carry their ids under a type of their own:
// the upload drop zone looks for "Files" and so leaves them alone.
export const NODES_MIME = "application/x-cloud-nodes";

export function isNodeDrag(dt: DataTransfer): boolean {
  return Array.from(dt.types).includes(NODES_MIME);
}

// draggedIds reads the ids off a drop; null when it isn't rows of ours.
export function draggedIds(dt: DataTransfer): string[] | null {
  if (!isNodeDrag(dt)) return null;
  try {
    const ids: unknown = JSON.parse(dt.getData(NODES_MIME));
    return isArrayOf(ids, isString) ? ids : null;
  } catch {
    return null;
  }
}

// The drag image stands in for the browser's snapshot, which would show
// only the row under the pointer: up to three cards stacked, the dragged
// one on top, and the count when there are more. Plain DOM, as the browser
// pictures it at once; a thumbnail shows if it's cached by then, else the
// card's icon block does.
function setStackImage(dt: DataTransfer, nodes: FileNode[], thumbnail?: (node: FileNode) => string | undefined) {
  const wrap = document.createElement("div");
  wrap.className = classes.dragImage;
  const stack = document.createElement("div");
  stack.className = classes.dragStack;
  const shown = nodes.slice(0, 3);
  // Bottom card first, so the next ones are drawn over it.
  for (let i = shown.length - 1; i >= 0; i--) {
    const n = shown[i];
    const card = document.createElement("div");
    card.className = classes.dragCard;
    card.style.transform = `translate(${i * 6}px, ${i * 6}px)`;
    const icon = document.createElement("span");
    icon.className = classes.dragIcon;
    if (n.kind === "dir") icon.dataset.folder = "";
    else icon.textContent = extension(n.name).slice(0, 4).toUpperCase();
    const src = thumbnail?.(n);
    if (src) {
      const img = document.createElement("img");
      img.src = src;
      img.alt = "";
      icon.append(img);
    }
    const name = document.createElement("span");
    name.className = classes.dragName;
    name.textContent = n.name;
    card.append(icon, name);
    stack.append(card);
  }
  if (nodes.length > 1) {
    const badge = document.createElement("span");
    badge.className = classes.dragCount;
    badge.textContent = t("files.dragCount", { count: nodes.length });
    stack.append(badge);
  }
  wrap.append(stack);
  document.body.append(wrap);
  // The image hangs below and right of the pointer by the wrap's
  // transparent padding, so the folder or crumb under it stays in view:
  // negative offsets would do the same, but browsers disagree on them.
  dt.setDragImage(wrap, 0, 0);
  // The browser takes its picture during dragstart.
  setTimeout(() => wrap.remove());
}

type Props = {
  items: FileNode[];
  sort: Sort;
  onSort: (key: SortKey) => void;
  // via says the keyboard opened it (Enter), for the page to keep the
  // focus on the list in the folder it goes to.
  onOpen: (node: FileNode, via?: "keyboard") => void;
  // Selection is the page's: its toolbar acts on it.
  selected: ReadonlySet<string>;
  onSelect: (ids: ReadonlySet<string>) => void;
  // Keyboard shortcuts: F2 renames the focused row, Delete deletes the
  // selection.
  onRename: (node: FileNode) => void;
  onDelete: () => void;
  hasMore: boolean;
  loadingMore: boolean;
  onLoadMore: () => void;
  // The last next page failed: the footer offers a retry instead of loading
  // on its own, which against a server that keeps failing would never stop.
  loadMoreError?: boolean;
  onRetryMore?: () => void;
  // thumbnail gives a file's small preview URL, if it may have one.
  thumbnail?: (node: FileNode) => string | undefined;
  // The page shows a menu on right-click (it listens around the list): the
  // list readies the selection for it and opens it from the keyboard.
  withContextMenu?: boolean;
  // Given, rows can be dragged onto folder rows to move there: the dragged
  // ids and the folder. canMoveInto tells which folders take them.
  onMoveInto?: (ids: string[], folder: FileNode) => void;
  canMoveInto?: (folder: FileNode) => boolean;
  // Rows with columns, or tiles of tileSize px wide at least, as many to a
  // line as fit.
  view?: ListView;
  tileSize?: number;
  // Up to the parent folder: Backspace or Alt+ArrowUp, and the ".." row
  // the list then starts with, there even when the folder is empty.
  onUp?: () => void;
  // Shown below the ".." row when there are no items.
  empty?: ReactNode;
  // Shown after keyboard navigation: the list takes the focus once loaded,
  // its first row (".." where there is one) active.
  autoFocus?: boolean;
  // Slots for the desktop client's window (desktop/frontend): check
  // replaces the selection checkbox of each item (and drops the header's
  // "select all"), badge goes after the name.
  check?: (node: FileNode) => ReactNode;
  badge?: (node: FileNode) => ReactNode;
};

const COLUMNS: { key: SortKey; label: MessageKey }[] = [
  { key: "name", label: "files.columns.name" },
  { key: "size", label: "files.columns.size" },
  { key: "mtime", label: "files.columns.mtime" },
];

export function NodeIcon({ node, size = 20 }: { node: FileNode; size?: number }) {
  const props = { size, stroke: 1.5, "aria-hidden": true };
  if (node.kind === "dir") return <IconFolderFilled {...props} className={classes.folderIcon} />;
  const mime = node.mime ?? "";
  if (mime.startsWith("image/")) return <IconPhoto {...props} />;
  if (mime.startsWith("video/")) return <IconMovie {...props} />;
  if (mime.startsWith("audio/")) return <IconMusic {...props} />;
  if (mime === "application/pdf") return <IconFileTypePdf {...props} />;
  if (mime.startsWith("text/")) return <IconFileText {...props} />;
  if (/zip|x-7z|x-rar|x-tar|gzip/.test(mime)) return <IconFileZip {...props} />;
  return <IconFile {...props} />;
}

// Thumb shows a file's preview in place of its icon; until it loads, and
// if there is none, the icon stays. Given iconSize, it fills its box (a
// tile's picture) and the icon is that big.
export function Thumb({ node, src, iconSize }: { node: FileNode; src: string; iconSize?: number }) {
  const [state, setState] = useState<"loading" | "ok" | "failed">("loading");
  if (state === "failed") return <NodeIcon node={node} size={iconSize} />;
  return (
    <span className={iconSize ? classes.tileThumb : classes.thumb}>
      {state === "loading" && <NodeIcon node={node} size={iconSize} />}
      <img
        src={src}
        alt=""
        // Its own native drag would carry the image as a file, which the
        // upload drop zone takes for one; the row is what's dragged.
        draggable={false}
        loading="lazy"
        decoding="async"
        data-loaded={state === "ok" || undefined}
        onLoad={() => setState("ok")}
        onError={() => setState("failed")}
      />
    </span>
  );
}

// One folder's contents as a virtualized list (R3: 50 000 items without
// stalling): only the rows on screen are in the DOM. Folders come first in
// every order — the backend sorts, the list only shows.
//
// Selection follows file-manager habits: click selects one, Ctrl/Cmd+click
// toggles, Shift+click extends from the last clicked row; the keyboard has
// arrows, Space (toggle), Enter (open), Ctrl/Cmd+A, F2 (rename), Delete and
// the menu key or Shift+F10 (the context menu). It covers the loaded rows.
//
// The grid view is the same list laid out in tiles: virtualized by lines of
// tiles, with the arrows moving across (left, right) and between lines (up,
// down); everything else works as in rows.
//
// By touch (told by the pointer, so a mouse keeps all of the above) a tap
// opens, a long press selects and starts selecting: taps then toggle until
// nothing is selected. Rows get a button for the context menu, which a
// finger can't right-click for.
export default function FileList(props: Props) {
  const { items, sort, onSort, onOpen, selected, onSelect: setSelected, onRename, onDelete, hasMore, loadingMore, onLoadMore, loadMoreError, onRetryMore, thumbnail } = props;
  const { withContextMenu = false, onMoveInto, canMoveInto, view = "list", tileSize = 180, onUp, autoFocus = false, check, badge, empty } = props;
  const grid = view === "grid";
  const phone = useIsPhone();
  const coarse = useCoarsePointer();
  const scrollRef = useRef<HTMLDivElement>(null);
  const [anchor, setAnchor] = useState<number | null>(null);
  // The active row's item index; -1 is the ".." row.
  const firstIndex = props.onUp ? -1 : 0;
  const [active, setActive] = useState(firstIndex);
  // The folder's path can load after its items: ".." appearing then makes
  // it the first row, and the active one.
  const [shownFirst, setShownFirst] = useState(firstIndex);
  if (shownFirst !== firstIndex) {
    setShownFirst(firstIndex);
    setActive(firstIndex);
  }
  // What's being dragged, to refuse dropping a folder into itself: a drop's
  // data can't be read before the drop.
  const [dragging, setDragging] = useState<ReadonlySet<string> | null>(null);
  const [dropTarget, setDropTarget] = useState<string | null>(null);
  // dragenter/leave fire for every child the pointer crosses; counting them
  // per row tells when it has really left that row.
  const dropDepth = useRef<{ id: string; depth: number } | null>(null);

  // The grid's width, for how many tiles fit a line; it follows the
  // window and the side panel.
  const [width, setWidth] = useState(0);
  useLayoutEffect(() => {
    const el = scrollRef.current;
    if (!grid || !el) return;
    const measure = () => setWidth(el.clientWidth);
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, [grid]);
  const cols = grid ? Math.max(1, Math.floor(width / tileSize)) : 1;
  const rowHeight = grid ? tileSize + TILE_EXTRA : phone ? PHONE_ROW_HEIGHT : ROW_HEIGHT;
  // A virtual row is a row of the list or a line of tiles; item i is in
  // row rowOf(i).
  // The ".." row, if any, is virtual row 0: a list row even in the grid.
  const upRows = onUp ? 1 : 0;
  const upHeight = phone ? PHONE_ROW_HEIGHT : ROW_HEIGHT;
  const rowOf = (index: number) => upRows + Math.floor(index / cols);
  const itemRows = Math.ceil(items.length / cols);

  const rowCount = upRows + itemRows + (hasMore ? 1 : 0);
  const virtualizer = useVirtualizer({
    count: rowCount,
    getScrollElement: () => scrollRef.current,
    estimateSize: (i) => (i < upRows ? upHeight : rowHeight),
    overscan: grid ? 3 : 10,
  });
  // Sizes are kept per row once worked out: a new height needs them anew.
  useEffect(() => {
    virtualizer.measure();
  }, [virtualizer, rowHeight, upRows, upHeight]);
  const virtualRows = virtualizer.getVirtualItems();
  const lastVisible = virtualRows.length > 0 ? virtualRows[virtualRows.length - 1].index : -1;

  useEffect(() => {
    if (hasMore && !loadingMore && !loadMoreError && lastVisible >= upRows + itemRows - PREFETCH_ROWS) onLoadMore();
  }, [hasMore, loadingMore, loadMoreError, lastVisible, itemRows, onLoadMore]);

  // Once only: a later refetch mustn't pull the focus back.
  const focused = useRef(false);
  useEffect(() => {
    if (!autoFocus || focused.current || (items.length === 0 && !onUp)) return;
    focused.current = true;
    setActive(firstIndex);
    virtualizer.scrollToIndex(0);
    scrollRef.current?.focus();
  }, [autoFocus, items, onUp, firstIndex, virtualizer]);

  const selectOnly = (index: number) => {
    setSelected(new Set([items[index].id]));
    setAnchor(index);
  };
  const toggle = (index: number) => {
    const next = new Set(selected);
    const id = items[index].id;
    if (next.has(id)) next.delete(id);
    else next.add(id);
    setSelected(next);
    setAnchor(index);
  };
  const extendTo = (index: number) => {
    const from = anchor ?? index;
    const [lo, hi] = from <= index ? [from, index] : [index, from];
    setSelected(new Set(items.slice(lo, hi + 1).map((n) => n.id)));
  };
  const allSelected = items.length > 0 && selected.size === items.length;
  const selectAll = () => setSelected(allSelected ? new Set() : new Set(items.map((n) => n.id)));

  // The touch in progress, or the last one, until its click reads it: a
  // click with none came from a mouse or the keyboard.
  const tap = useRef<{ x: number; y: number; long: boolean } | null>(null);
  const pressTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const lastPointer = useRef("");
  const [selecting, setSelecting] = useState(false);
  // Emptied, by taps or the toolbar, the selection ends selecting.
  if (selecting && selected.size === 0) setSelecting(false);
  useEffect(() => () => clearTimeout(pressTimer.current), []);

  const pointerDown = (e: PointerEvent, index: number) => {
    lastPointer.current = e.pointerType;
    clearTimeout(pressTimer.current);
    if (e.pointerType !== "touch") {
      tap.current = null;
      return;
    }
    const press = { x: e.clientX, y: e.clientY, long: false };
    tap.current = press;
    pressTimer.current = setTimeout(() => {
      press.long = true;
      setActive(index);
      setSelecting(true);
      if (!selected.has(items[index].id)) toggle(index);
    }, LONG_PRESS_MS);
  };
  // Moved, it's a scroll, not a press.
  const pointerMove = (e: PointerEvent) => {
    const press = tap.current;
    if (press && Math.hypot(e.clientX - press.x, e.clientY - press.y) > LONG_PRESS_SLOP) clearTimeout(pressTimer.current);
  };
  const pointerUp = () => clearTimeout(pressTimer.current);

  const click = (e: MouseEvent, index: number) => {
    const touch = tap.current;
    tap.current = null;
    if (touch) {
      // The long press has done its part already.
      if (touch.long) return;
      setActive(index);
      if (selecting) toggle(index);
      else onOpen(items[index]);
      return;
    }
    setActive(index);
    if (e.shiftKey) extendTo(index);
    else if (e.metaKey || e.ctrlKey) toggle(index);
    else selectOnly(index);
  };

  // On a Mac, Ctrl+click is the system's right-click: the browser sends
  // contextmenu and no click. Here Ctrl selects, as everywhere else.
  const rowContextMenu = (e: MouseEvent, index: number) => {
    // A long press makes one too (Android): it selects instead, and the
    // native menu and the page's stay shut. The row's menu button opens
    // the page's menu once the finger is up.
    if (tap.current) {
      e.preventDefault();
      return;
    }
    if (e.button === 0 && e.ctrlKey) {
      e.preventDefault();
      click(e, index);
      return;
    }
    if (!withContextMenu) return;
    setActive(index);
    // As in Finder and Explorer: the menu acts on the selection the row is
    // in, or else on the row alone.
    if (!selected.has(items[index].id)) selectOnly(index);
  };
  // Below the rows: the folder's menu, so nothing stays selected. The menu
  // key and Shift+F10 also land here, on the focused list, and are sent on
  // to the active row.
  const listContextMenu = (e: MouseEvent) => {
    // Between tiles counts as below the rows.
    const background = e.target === e.currentTarget || (e.target instanceof Element && e.target.hasAttribute("data-list-gap"));
    if (!withContextMenu || !background) return;
    const native = e.nativeEvent;
    const pointer = "pointerType" in native && native.pointerType !== "";
    if (e.button === 0 && e.ctrlKey) e.preventDefault();
    else if (e.button !== 2 && !pointer && items.length > 0 && active >= 0) {
      e.preventDefault();
      menuFromRow(active);
    } else setSelected(new Set());
  };
  // Opens the page's menu by a row the way a right-click on it would,
  // so both take the same path.
  const menuFromRow = (index: number, retry = true) => {
    const row = document.getElementById(`node-${items[index].id}`);
    if (row) {
      const r = row.getBoundingClientRect();
      // The menu opens at a point: below the row, or for a row low on the
      // screen at its top edge, so that the menu, flipped up for lack of
      // room, doesn't cover the row.
      const y = r.top + r.height / 2 < window.innerHeight / 2 ? r.bottom : r.top;
      row.dispatchEvent(new globalThis.MouseEvent("contextmenu", { bubbles: true, cancelable: true, clientX: r.left + 40, clientY: y }));
    } else if (retry) {
      virtualizer.scrollToIndex(rowOf(index));
      requestAnimationFrame(() => menuFromRow(index, false));
    }
  };

  const canDrop = (n: FileNode) => n.kind === "dir" && !dragging?.has(n.id) && (canMoveInto?.(n) ?? true);
  const endDrag = () => {
    setDragging(null);
    setDropTarget(null);
    dropDepth.current = null;
  };
  // Dragging a row outside the selection drags it alone (and selects it);
  // a selected row brings the rest of the selection along.
  const dragStart = (e: DragEvent, index: number) => {
    if (!onMoveInto) return;
    const node = items[index];
    const ids = selected.has(node.id) ? items.filter((n) => selected.has(n.id)).map((n) => n.id) : [node.id];
    if (!selected.has(node.id)) selectOnly(index);
    setActive(index);
    setDragging(new Set(ids));
    e.dataTransfer.effectAllowed = "move";
    e.dataTransfer.setData(NODES_MIME, JSON.stringify(ids));
    setStackImage(e.dataTransfer, [node, ...items.filter((n) => n.id !== node.id && ids.includes(n.id))], thumbnail);
  };
  const dragEnter = (e: DragEvent, node: FileNode) => {
    if (!isNodeDrag(e.dataTransfer)) return;
    if (dropDepth.current?.id !== node.id) dropDepth.current = { id: node.id, depth: 0 };
    dropDepth.current.depth++;
    setDropTarget(canDrop(node) ? node.id : null);
  };
  const dragLeave = (node: FileNode) => {
    const at = dropDepth.current;
    if (at?.id !== node.id || --at.depth > 0) return;
    dropDepth.current = null;
    setDropTarget(null);
  };
  const dragOver = (e: DragEvent, node: FileNode) => {
    // Not cancelled, the drop isn't allowed: the browser shows as much.
    if (!isNodeDrag(e.dataTransfer) || !canDrop(node)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "move";
  };
  const dropOn = (e: DragEvent, node: FileNode) => {
    const ids = draggedIds(e.dataTransfer);
    endDrag();
    if (!ids || !onMoveInto || !canDrop(node) || ids.includes(node.id)) return;
    e.preventDefault();
    onMoveInto(ids, node);
  };

  const moveTo = (index: number, extend: boolean) => {
    const i = Math.max(firstIndex, Math.min(items.length - 1, index));
    setActive(i);
    virtualizer.scrollToIndex(i < 0 ? 0 : rowOf(i));
    // On ".." nothing is selected: it is a way out, not an item.
    if (i < 0) {
      if (!extend) setSelected(new Set());
    } else if (extend) extendTo(i);
    else selectOnly(i);
  };
  const keyDown = (e: KeyboardEvent) => {
    if (items.length === 0 && !onUp) return;
    const page = Math.max(1, Math.floor((scrollRef.current?.clientHeight ?? rowHeight) / rowHeight) - 1) * cols;
    // From "..", a line of its own, down goes to the first item.
    const down = active < 0 ? 0 : active + cols;
    const moves: Record<string, number> = {
      ArrowDown: down,
      ArrowUp: active < cols ? firstIndex : active - cols,
      PageDown: active < 0 ? page - 1 : active + page,
      PageUp: active - page,
      Home: firstIndex,
      End: items.length - 1,
      ...(grid ? { ArrowRight: active + 1, ArrowLeft: active - 1 } : {}),
    };
    if (onUp && ((e.key === "Backspace" && !e.metaKey && !e.ctrlKey && !e.altKey && !e.shiftKey) || (e.key === "ArrowUp" && e.altKey))) {
      e.preventDefault();
      onUp();
    } else if (e.key in moves) {
      e.preventDefault();
      moveTo(moves[e.key], e.shiftKey);
    } else if (active < 0) {
      // The rest act on an item.
      if (e.key === "Enter" && onUp) {
        e.preventDefault();
        onUp();
      } else if (e.key === "a" && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        setSelected(new Set(items.map((n) => n.id)));
      } else if (e.key === "Escape") setSelected(new Set());
    } else if (e.key === "Enter") {
      e.preventDefault();
      onOpen(items[active], "keyboard");
    } else if (e.key === " ") {
      e.preventDefault();
      toggle(active);
    } else if (e.key === "a" && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      setSelected(new Set(items.map((n) => n.id)));
    } else if (e.key === "Escape") {
      setSelected(new Set());
    } else if (withContextMenu && (e.key === "ContextMenu" || (e.key === "F10" && e.shiftKey))) {
      e.preventDefault();
      // Once this key is through: the open menu takes a keydown outside it
      // for a click outside, and would close at once.
      const index = active;
      setTimeout(() => menuFromRow(index));
    } else if (e.key === "F2") {
      e.preventDefault();
      onRename(items[active]);
    } else if (e.key === "Delete" || (e.key === "Backspace" && e.metaKey)) {
      // Cmd+Backspace is the Mac's "move to trash".
      e.preventDefault();
      if (selected.size > 0) onDelete();
    }
  };

  // What rows and tiles share: selection, opening, the menu, dragging.
  const optionProps = (node: FileNode, index: number) => {
    const isSelected = selected.has(node.id);
    return {
      id: `node-${node.id}`,
      role: "option",
      "aria-selected": isSelected,
      "data-selected": isSelected || undefined,
      "data-active": index === active || undefined,
      "data-drop-target": dropTarget === node.id || undefined,
      onClick: (e: MouseEvent) => click(e, index),
      // By touch the first tap has opened it.
      onDoubleClick: () => lastPointer.current !== "touch" && onOpen(node),
      onPointerDown: (e: PointerEvent) => pointerDown(e, index),
      onPointerMove: pointerMove,
      onPointerUp: pointerUp,
      onPointerCancel: pointerUp,
      // The page's menu opens on a long press of its own (Menu.ContextMenu);
      // on a row the long press selects.
      onTouchStart: (e: TouchEvent) => e.stopPropagation(),
      onContextMenu: (e: MouseEvent) => rowContextMenu(e, index),
      draggable: onMoveInto !== undefined,
      onDragStart: (e: DragEvent) => dragStart(e, index),
      onDragEnd: endDrag,
      onDragEnter: (e: DragEvent) => dragEnter(e, node),
      onDragLeave: () => dragLeave(node),
      onDragOver: (e: DragEvent) => dragOver(e, node),
      onDrop: (e: DragEvent) => dropOn(e, node),
    };
  };
  const checkbox = (node: FileNode, index: number) =>
    check ? (
      check(node)
    ) : (
      <Checkbox
        aria-label={t("files.select", { name: node.name })}
        checked={selected.has(node.id)}
        onChange={() => {
          // Ticked by touch, it starts selecting, as a long press does.
          if (tap.current) setSelecting(true);
          tap.current = null;
          toggle(index);
        }}
        size="xs"
        tabIndex={-1}
      />
    );
  // The context menu for a finger: what a right-click on the row opens.
  const menuButton = (node: FileNode, index: number) =>
    withContextMenu &&
    coarse && (
      <ActionIcon
        variant="subtle"
        color="gray"
        className={grid ? classes.tileMenu : classes.rowMenu}
        aria-label={t("files.actionsFor", { name: node.name })}
        // Not a press of the row: no long press, no selection by tap.
        onPointerDown={(e) => e.stopPropagation()}
        onTouchStart={(e) => e.stopPropagation()}
        onDoubleClick={(e) => e.stopPropagation()}
        onClick={(e) => {
          e.stopPropagation();
          tap.current = null;
          menuFromRow(index);
        }}
      >
        <IconDots size={18} />
      </ActionIcon>
    );
  const iconSize = Math.round(tileSize * 0.4);

  return (
    <Box className={classes.root}>
      {!grid && (
        <Group className={classes.header} gap={0} wrap="nowrap">
          <Box className={classes.check}>
            {!check && (
              <Checkbox
                aria-label={t("files.selectAll")}
                checked={allSelected}
                indeterminate={selected.size > 0 && !allSelected}
                onChange={selectAll}
                size="xs"
              />
            )}
          </Box>
          {/* A phone shows size and date under the name, sorted by from the toolbar. */}
          {(phone ? COLUMNS.slice(0, 1) : COLUMNS).map((c) => (
            <UnstyledButton key={c.key} className={classes[c.key]} onClick={() => onSort(c.key)} aria-label={t("files.sortBy", { column: t(c.label) })}>
              <Group gap={4} wrap="nowrap" justify={c.key === "name" ? "flex-start" : "flex-end"}>
                <Text size="sm" fw={500} c="dimmed">
                  {t(c.label)}
                </Text>
                {sort.key === c.key &&
                  (sort.order === "asc" ? <IconChevronUp size={14} aria-hidden /> : <IconChevronDown size={14} aria-hidden />)}
              </Group>
            </UnstyledButton>
          ))}
        </Group>
      )}
      <div
        ref={scrollRef}
        className={classes.scroll}
        role="listbox"
        aria-multiselectable
        aria-label={t("files.listLabel")}
        data-selecting={selecting || undefined}
        aria-activedescendant={active < 0 ? "node-up" : items[active] ? `node-${items[active].id}` : undefined}
        tabIndex={0}
        onKeyDown={keyDown}
        onContextMenu={listContextMenu}
      >
        <div data-list-gap style={{ height: virtualizer.getTotalSize(), position: "relative" }}>
          {virtualRows.map((virtualRow) => {
            if (onUp && virtualRow.index === 0) {
              return (
                <div
                  key="up"
                  id="node-up"
                  data-up
                  role="option"
                  data-active={active < 0 || undefined}
                  aria-selected={false}
                  aria-label={t("files.up")}
                  title={t("files.up")}
                  className={classes.row}
                  style={{ transform: `translateY(${virtualRow.start}px)`, height: upHeight }}
                  onPointerDown={(e) => (lastPointer.current = e.pointerType)}
                  // As a folder opens: by double click, or by a tap.
                  onClick={() => (lastPointer.current === "touch" ? onUp() : setActive(-1))}
                  onDoubleClick={() => lastPointer.current !== "touch" && onUp()}
                >
                  <Box className={classes.check} />
                  <Group className={classes.name} gap="sm" wrap="nowrap">
                    <IconFolderFilled size={20} stroke={1.5} aria-hidden className={classes.folderIcon} />
                    <Text size="sm">..</Text>
                  </Group>
                </div>
              );
            }
            const row = { ...virtualRow, index: virtualRow.index - upRows };
            if (row.index >= itemRows) {
              return (
                <Group key="loading" className={classes.row} style={{ transform: `translateY(${row.start}px)` }} justify="center">
                  {loadingMore ? (
                    <Loader size="sm" aria-label={t("common.loading")} />
                  ) : (
                    loadMoreError && (
                      <>
                        <Text size="sm" c="red">
                          {t("files.moreFailed")}
                        </Text>
                        <Button size="compact-sm" variant="subtle" onClick={onRetryMore}>
                          {t("common.retry")}
                        </Button>
                      </>
                    )
                  )}
                </Group>
              );
            }
            if (grid) {
              const first = row.index * cols;
              return (
                <div
                  key={row.index}
                  data-list-gap
                  className={classes.gridRow}
                  style={{ transform: `translateY(${row.start}px)`, height: rowHeight, gridTemplateColumns: `repeat(${cols}, minmax(0, 1fr))` }}
                >
                  {items.slice(first, first + cols).map((node, i) => {
                    const thumb = thumbnail?.(node);
                    return (
                      <div key={node.id} {...optionProps(node, first + i)} className={classes.tile}>
                        <Box className={classes.tileCheck} onClick={(e) => e.stopPropagation()}>
                          {checkbox(node, first + i)}
                        </Box>
                        <div className={classes.tilePicture}>
                          {thumb ? <Thumb key={node.etag} node={node} src={thumb} iconSize={iconSize} /> : <NodeIcon node={node} size={iconSize} />}
                        </div>
                        <Text size="sm" ta="center" lineClamp={2} title={node.name} className={classes.tileName}>
                          {node.name}
                        </Text>
                        {badge?.(node)}
                        {menuButton(node, first + i)}
                      </div>
                    );
                  })}
                </div>
              );
            }
            const node = items[row.index];
            const thumb = thumbnail?.(node);
            return (
              <div
                key={node.id}
                {...optionProps(node, row.index)}
                className={classes.row}
                style={{ transform: `translateY(${row.start}px)`, ...(phone && { height: PHONE_ROW_HEIGHT }) }}
              >
                <Box className={classes.check} onClick={(e) => e.stopPropagation()}>
                  {checkbox(node, row.index)}
                </Box>
                <Group className={classes.name} gap="sm" wrap="nowrap">
                  {thumb ? <Thumb key={node.etag} node={node} src={thumb} /> : <NodeIcon node={node} />}
                  {phone ? (
                    <Box miw={0}>
                      <Text size="sm" truncate="end" title={node.name}>
                        {node.name}
                      </Text>
                      <Text size="xs" c="dimmed" truncate="end">
                        {formatBytes(node.size)} · {formatDate(node.mtime)}
                      </Text>
                    </Box>
                  ) : (
                    <Text size="sm" truncate="end" title={node.name}>
                      {node.name}
                    </Text>
                  )}
                  {badge?.(node)}
                </Group>
                {!phone && (
                  <>
                    <Text size="sm" c="dimmed" className={classes.size}>
                      {formatBytes(node.size)}
                    </Text>
                    <Text size="sm" c="dimmed" className={classes.mtime}>
                      {formatDate(node.mtime)}
                    </Text>
                  </>
                )}
                {menuButton(node, row.index)}
              </div>
            );
          })}
        </div>
        {items.length === 0 && !hasMore && empty}
      </div>
    </Box>
  );
}
