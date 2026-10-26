import { ActionIcon, Alert, Badge, Box, Button, Checkbox, Group, List, Modal, Progress, ScrollArea, Stack, Text } from "@mantine/core";
import { IconAlertTriangle, IconChevronDown, IconChevronRight, IconFolder } from "@tabler/icons-react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, errorText } from "../api";
import { fetchMe, meQueryKey } from "../auth";
import { fetchChildren, type FileNode } from "../files";
import { formatBytes, formatDate, t } from "../i18n";
import { PermissionLost, type Outcome, type Progress as PassProgress } from "../websync/execute";
import type { DirHandle } from "../websync/localfs";
import { conflictStamp } from "../websync/names";
import type { Problem } from "../websync/plan";
import { fetchRoots, type SyncRoot } from "../websync/remote";
import { checkState, SHARED_ID, toggle, type Place, type Rule } from "../websync/selection";
import { IdbStore, MemoryStore, type Config, type SyncStore } from "../websync/store";
import {
  acquire,
  ensurePermission,
  FolderGone,
  FolderLooksEmpty,
  prepare,
  run,
  serverApi,
  type Deletion,
  type PassContext,
  type Prepared,
  type Stage,
} from "../websync/sync";

// The browser can sync with a local folder: Chromium on a computer, over
// https (or localhost). Looked up when asked, not cached.
export function webSyncSupported(): boolean {
  return typeof window !== "undefined" && typeof window.showDirectoryPicker === "function";
}

let sharedStore: SyncStore | undefined;

// IndexedDB where there is one; without it (some private modes) every pass
// starts from scratch, which costs hashing but loses nothing.
function store(): SyncStore {
  sharedStore ??= typeof indexedDB === "undefined" ? new MemoryStore() : new IdbStore();
  return sharedStore;
}

type State =
  | { kind: "idle" }
  | { kind: "preparing"; stage: Stage }
  | { kind: "confirm"; prepared: Prepared; ctx: PassContext }
  | { kind: "running"; progress: PassProgress }
  | { kind: "done"; outcome: Outcome; stopped: boolean }
  | { kind: "stopped" }
  | { kind: "error"; message: string };

const LIST_MAX = 50;

function isAbort(err: unknown): boolean {
  return err instanceof DOMException && err.name === "AbortError";
}

function failure(err: unknown): string {
  if (err instanceof PermissionLost) return t("webSync.errors.permission");
  if (err instanceof FolderGone) return t("webSync.errors.gone");
  if (err instanceof FolderLooksEmpty) return t("webSync.errors.empty", { count: err.synced });
  if (err instanceof DOMException && (err.name === "NotAllowedError" || err.name === "SecurityError")) return t("webSync.errors.permission");
  if (err instanceof DOMException && err.name === "NotFoundError") return t("webSync.errors.gone");
  return t("webSync.errors.failed", { error: errorText(err) });
}

function problemText(p: Problem): string {
  if (p.kind === "failed") return t("webSync.problem.failed", { error: errorText(new ApiError(0, p.code ?? "unknown", "")) });
  return t(`webSync.problem.${p.kind}`);
}

// Every folder in a folder: they come first in a listing by name, so the
// first file ends it.
async function fetchFolders(id: string, signal?: AbortSignal): Promise<FileNode[]> {
  const out: FileNode[] = [];
  let cursor: string | null = null;
  do {
    const page = await fetchChildren(id, { key: "name", order: "asc" }, cursor, signal);
    out.push(...page.items.filter((n) => n.kind === "dir"));
    if (page.items.some((n) => n.kind === "file")) break;
    cursor = page.nextCursor;
  } while (cursor);
  return out;
}

function FolderRow({
  place,
  name,
  readOnly,
  rules,
  depth,
  onToggle,
  children,
}: {
  place: Place;
  name: string;
  readOnly?: boolean;
  rules: Rule[];
  depth: number;
  onToggle: (p: Place) => void;
  // Fixed children (the shares under «Доступно мне»); a folder's are
  // listed from the server when it is opened.
  children?: (chain: string[]) => React.ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const kids = useQuery({
    queryKey: ["syncFolders", place.id],
    queryFn: ({ signal }) => fetchFolders(place.id, signal),
    enabled: open && !children,
    retry: false,
  });
  const state = checkState(rules, place);
  const chain = [...place.chain, place.id];
  return (
    <>
      <Group gap={4} wrap="nowrap" pl={depth * 22}>
        <ActionIcon
          variant="subtle"
          color="gray"
          size="sm"
          aria-label={t(open ? "webSync.collapse" : "webSync.expand", { name })}
          aria-expanded={open}
          onClick={() => setOpen(!open)}
        >
          {open ? <IconChevronDown size={14} /> : <IconChevronRight size={14} />}
        </ActionIcon>
        <Checkbox
          size="sm"
          checked={state === "checked"}
          indeterminate={state === "partial"}
          onChange={() => onToggle(place)}
          label={
            <Group gap={6} wrap="nowrap">
              <IconFolder size={16} stroke={1.5} aria-hidden />
              <Text size="sm" truncate="end">
                {name}
              </Text>
              {readOnly && (
                <Badge size="xs" variant="light" color="gray">
                  {t("webSync.readOnly")}
                </Badge>
              )}
            </Group>
          }
        />
      </Group>
      {open && children?.(chain)}
      {open && !children && kids.isPending && (
        <Text size="xs" c="dimmed" pl={(depth + 1) * 22 + 28}>
          {t("common.loading")}
        </Text>
      )}
      {open && !children && kids.data?.length === 0 && (
        <Text size="xs" c="dimmed" pl={(depth + 1) * 22 + 28}>
          {t("webSync.noSubfolders")}
        </Text>
      )}
      {open &&
        kids.data?.map((k) => (
          <FolderRow
            key={k.id}
            place={{ id: k.id, root: place.root, chain }}
            name={k.name}
            readOnly={!k.permissions.includes("write")}
            rules={rules}
            depth={depth + 1}
            onToggle={onToggle}
          />
        ))}
    </>
  );
}

function DeletionList({ title, items }: { title: string; items: Deletion[] }) {
  if (items.length === 0) return null;
  return (
    <Stack gap={4}>
      <Text size="sm" fw={600}>
        {title}
      </Text>
      <List size="xs" spacing={2}>
        {items.slice(0, LIST_MAX).map((d) => (
          <List.Item key={d.path}>{d.kind === "dir" ? t("webSync.deletes.folder", { path: d.path }) : d.path}</List.Item>
        ))}
      </List>
      {items.length > LIST_MAX && (
        <Text size="xs" c="dimmed">
          {t("webSync.deletes.more", { count: items.length - LIST_MAX })}
        </Text>
      )}
    </Stack>
  );
}

function SummaryLines({ prepared }: { prepared: Prepared }) {
  const s = prepared.summary;
  const lines = [
    s.upload > 0 && t("webSync.summary.upload", { count: s.upload, size: formatBytes(s.uploadBytes) }),
    s.download > 0 && t("webSync.summary.download", { count: s.download, size: formatBytes(s.downloadBytes) }),
    s.conflicts > 0 && t("webSync.summary.conflicts", { count: s.conflicts }),
  ].filter((x): x is string => typeof x === "string");
  return (
    <Stack gap={2}>
      {lines.length === 0 && s.deleteLocal.length + s.deleteRemote.length === 0 ? (
        <Text size="sm">{t("webSync.summary.nothing")}</Text>
      ) : (
        lines.map((l) => (
          <Text key={l} size="sm">
            {l}
          </Text>
        ))
      )}
    </Stack>
  );
}

function OutcomeView({ outcome, stopped }: { outcome: Outcome; stopped: boolean }) {
  const counts = (
    [
      ["uploaded", outcome.uploaded],
      ["downloaded", outcome.downloaded],
      ["conflicts", outcome.conflicts],
      ["deletedRemote", outcome.deletedRemote],
      ["deletedLocal", outcome.deletedLocal],
      ["deferred", outcome.deferred],
    ] satisfies [string, number][]
  ).filter(([, n]) => n > 0);
  return (
    <Stack gap="xs">
      <Alert color={stopped ? "yellow" : outcome.problems.length > 0 ? "orange" : "green"} title={stopped ? undefined : t("webSync.result.done")}>
        {stopped && <Text size="sm">{t("webSync.result.stopped")}</Text>}
        {counts.map(([k, n]) => (
          <Text key={k} size="sm">
            {k === "uploaded"
              ? t("webSync.result.uploaded", { count: n })
              : k === "downloaded"
                ? t("webSync.result.downloaded", { count: n })
                : k === "conflicts"
                  ? t("webSync.result.conflicts", { count: n })
                  : k === "deletedRemote"
                    ? t("webSync.result.deletedRemote", { count: n })
                    : k === "deletedLocal"
                      ? t("webSync.result.deletedLocal", { count: n })
                      : t("webSync.result.deferred", { count: n })}
          </Text>
        ))}
        {counts.length === 0 && outcome.problems.length === 0 && !stopped && <Text size="sm">{t("webSync.summary.nothing")}</Text>}
      </Alert>
      {outcome.problems.length > 0 && (
        <Stack gap={4}>
          <Text size="sm" fw={600}>
            {t("webSync.result.problems", { count: outcome.problems.length })}
          </Text>
          <List size="xs" spacing={2}>
            {outcome.problems.slice(0, LIST_MAX).map((p, i) => (
              <List.Item key={`${p.path}:${i}`}>
                <Text span size="xs" fw={500}>
                  {p.path}
                </Text>
                {" — "}
                {problemText(p)}
              </List.Item>
            ))}
          </List>
          {outcome.problems.length > LIST_MAX && (
            <Text size="xs" c="dimmed">
              {t("webSync.result.more", { count: outcome.problems.length - LIST_MAX })}
            </Text>
          )}
        </Stack>
      )}
    </Stack>
  );
}

function stageText(s: Stage): string {
  switch (s.stage) {
    case "remote":
      return t("webSync.stage.remote");
    case "scan":
      return t("webSync.stage.scan", { count: s.items });
    case "hash":
      return t("webSync.stage.hash", { done: s.done, total: s.total });
    case "plan":
      return t("webSync.stage.plan");
  }
}

function conflictTag(n: number): string {
  const params = { device: t("webSync.device"), date: conflictStamp(new Date()) };
  return n > 1 ? t("webSync.conflictTagN", { ...params, n }) : t("webSync.conflictTag", params);
}

function isWindows(): boolean {
  return /Windows/i.test(navigator.userAgent);
}

// The window of the browser's folder sync: the local folder, the cloud
// folders to sync, and one pass per press of «Синхронизировать».
export default function WebSyncDialog({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const queryClient = useQueryClient();
  const me = useQuery({ queryKey: meQueryKey, queryFn: ({ signal }) => fetchMe(signal), staleTime: Infinity, retry: false });
  const userId = me.data?.id;
  const [config, setConfig] = useState<Config | null>(null);
  const [state, setState] = useState<State>({ kind: "idle" });
  const [massChecked, setMassChecked] = useState(false);
  const abort = useRef<AbortController | null>(null);
  const release = useRef<(() => void) | null>(null);
  // Progress arrives per chunk: the window shows it a few times a second.
  const latest = useRef<PassProgress | null>(null);

  const roots = useQuery({ queryKey: ["syncRoots"], queryFn: ({ signal }) => fetchRoots(signal), enabled: opened, retry: false });

  useEffect(() => {
    if (!opened || !userId || config?.userId === userId) return;
    let live = true;
    store()
      .config(userId)
      .catch(() => ({ userId, rules: [] }))
      .then((c) => live && setConfig(c));
    return () => {
      live = false;
    };
  }, [opened, userId, config?.userId]);

  const save = useCallback((c: Config) => {
    setConfig(c);
    void store()
      .saveConfig(c)
      .catch(() => undefined);
  }, []);

  const busy = state.kind === "preparing" || state.kind === "running" || state.kind === "confirm";

  // Leaving the page would cut a pass off; the browser asks first.
  useEffect(() => {
    if (state.kind !== "running" && state.kind !== "preparing") return;
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [state.kind]);

  useEffect(() => {
    if (state.kind !== "running") return;
    const id = window.setInterval(() => {
      const p = latest.current;
      if (p) setState((s) => (s.kind === "running" ? { kind: "running", progress: p } : s));
    }, 200);
    return () => window.clearInterval(id);
  }, [state.kind]);

  const finish = useCallback(() => {
    release.current?.();
    release.current = null;
    abort.current = null;
    // What the pass changed shows in the open lists.
    void queryClient.invalidateQueries({ queryKey: ["children"] });
    void queryClient.invalidateQueries({ queryKey: ["node"] });
  }, [queryClient]);

  const pick = async () => {
    if (!config || !window.showDirectoryPicker) return;
    let handle: DirHandle;
    try {
      handle = await window.showDirectoryPicker({ id: "cloud-websync", mode: "readwrite" });
    } catch (err) {
      if (!isAbort(err)) setState({ kind: "error", message: t("webSync.pickFailed", { error: String(err) }) });
      return;
    }
    const old = config.folder;
    const same = old ? await handle.isSameEntry(old.handle).catch(() => false) : false;
    if (old && !same) await store().clearBase(old.id).catch(() => undefined);
    save({ ...config, folder: { handle, name: handle.name, id: same && old ? old.id : crypto.randomUUID() } });
    setState({ kind: "idle" });
  };

  const execute = async (ctx: PassContext, prepared: Prepared, deletes: boolean) => {
    latest.current = null;
    setState({ kind: "running", progress: { done: 0, total: prepared.plan.ops.length, bytes: 0, totalBytes: 0 } });
    try {
      const outcome = await run(ctx, prepared, deletes, (p) => {
        latest.current = p;
      });
      if (config) save({ ...config, lastSync: new Date().toISOString() });
      setState({ kind: "done", outcome, stopped: false });
    } catch (err) {
      setState(isAbort(err) ? { kind: "stopped" } : { kind: "error", message: failure(err) });
    } finally {
      finish();
    }
  };

  const start = async () => {
    const folder = config?.folder;
    if (!config || !folder) return;
    // First, while the click still counts as the user's: a permission
    // prompt needs it.
    let granted = false;
    try {
      granted = await ensurePermission(folder.handle);
    } catch {
      granted = false;
    }
    if (!granted) {
      setState({ kind: "error", message: t("webSync.errors.permission") });
      return;
    }
    const unlock = await acquire();
    if (!unlock) {
      setState({ kind: "error", message: t("webSync.busy") });
      return;
    }
    release.current = unlock;
    const ctl = new AbortController();
    abort.current = ctl;
    const ctx: PassContext = {
      store: store(),
      root: folder.handle,
      folderId: folder.id,
      rules: config.rules,
      sharedName: t("webSync.sharedFolder"),
      conflictTag,
      windows: isWindows(),
      api: serverApi,
      signal: ctl.signal,
    };
    setMassChecked(false);
    setState({ kind: "preparing", stage: { stage: "remote" } });
    let prepared: Prepared;
    try {
      prepared = await prepare(ctx, (stage) => setState({ kind: "preparing", stage }));
    } catch (err) {
      setState(isAbort(err) ? { kind: "idle" } : { kind: "error", message: failure(err) });
      finish();
      return;
    }
    if (prepared.summary.deleteRemote.length + prepared.summary.deleteLocal.length > 0) {
      // The lock stays held while the user reads the list.
      setState({ kind: "confirm", prepared, ctx });
      return;
    }
    await execute(ctx, prepared, false);
  };

  const close = () => {
    if (state.kind === "confirm") {
      finish();
      setState({ kind: "idle" });
    } else if (!busy) {
      setState({ kind: "idle" });
    }
    onClose();
  };

  const own = roots.data?.find((r) => r.kind === "own");
  const shares = roots.data?.filter((r): r is SyncRoot => r.kind === "shared") ?? [];
  const rules = config?.rules ?? [];
  const onToggle = (p: Place) => config && save({ ...config, rules: toggle(rules, p) });
  const deletions = state.kind === "confirm" ? state.prepared.summary.deleteRemote.length + state.prepared.summary.deleteLocal.length : 0;

  return (
    <Modal opened={opened} onClose={close} title={t("webSync.title")} size="lg" closeButtonProps={{ "aria-label": t("webSync.close") }}>
      <Stack gap="md">
        <Text size="sm" c="dimmed">
          {t("webSync.intro")}
        </Text>

        <Group justify="space-between" wrap="nowrap">
          <Box miw={0}>
            <Text size="sm" fw={600}>
              {t("webSync.folderLabel")}
            </Text>
            <Text size="sm" truncate="end">
              {config?.folder ? config.folder.name : t("webSync.noFolder")}
            </Text>
          </Box>
          <Button variant="default" size="xs" onClick={() => void pick()} disabled={busy || !config}>
            {config?.folder ? t("webSync.changeFolder") : t("webSync.chooseFolder")}
          </Button>
        </Group>

        <Stack gap={4}>
          <Text size="sm" fw={600}>
            {t("webSync.cloudLabel")}
          </Text>
          <Text size="xs" c="dimmed">
            {t("webSync.layoutHint", { shared: t("webSync.sharedFolder") })}
          </Text>
          <ScrollArea.Autosize mah={260} type="auto">
            <Stack gap={2} py={4}>
              {roots.isPending && (
                <Text size="sm" c="dimmed">
                  {t("common.loading")}
                </Text>
              )}
              {roots.isError && (
                <Text size="sm" c="red">
                  {errorText(roots.error)}
                </Text>
              )}
              {own && <FolderRow place={{ id: own.id, root: own.id, chain: [] }} name={t("webSync.myFiles")} rules={rules} depth={0} onToggle={onToggle} />}
              {shares.length > 0 && (
                <FolderRow place={{ id: SHARED_ID, root: SHARED_ID, chain: [] }} name={t("webSync.sharedFolder")} rules={rules} depth={0} onToggle={onToggle}>
                  {(chain) =>
                    shares.map((s) => (
                      <FolderRow
                        key={s.id}
                        place={{ id: s.id, root: s.id, chain }}
                        name={s.name}
                        readOnly={!s.permissions.includes("write")}
                        rules={rules}
                        depth={1}
                        onToggle={onToggle}
                      />
                    ))
                  }
                </FolderRow>
              )}
            </Stack>
          </ScrollArea.Autosize>
        </Stack>

        {state.kind === "preparing" && (
          <Group gap="xs">
            <Text size="sm">{stageText(state.stage)}</Text>
          </Group>
        )}

        {state.kind === "confirm" && (
          <Stack gap="sm">
            <SummaryLines prepared={state.prepared} />
            <Alert color={state.prepared.summary.mass ? "red" : "yellow"} icon={<IconAlertTriangle size={18} />} title={t("webSync.deletes.title")}>
              <Stack gap="sm">
                <Text size="sm">{t("webSync.deletes.text")}</Text>
                {state.prepared.summary.mass && (
                  <Text size="sm" fw={600}>
                    {t("webSync.deletes.mass", { count: deletions })}
                  </Text>
                )}
                <ScrollArea.Autosize mah={220} type="auto">
                  <Stack gap="sm">
                    <DeletionList title={t("webSync.deletes.remote", { count: state.prepared.summary.deleteRemote.length })} items={state.prepared.summary.deleteRemote} />
                    <DeletionList title={t("webSync.deletes.local", { count: state.prepared.summary.deleteLocal.length })} items={state.prepared.summary.deleteLocal} />
                  </Stack>
                </ScrollArea.Autosize>
                {state.prepared.summary.mass && (
                  <Checkbox label={t("webSync.deletes.checked")} checked={massChecked} onChange={(e) => setMassChecked(e.currentTarget.checked)} />
                )}
                <Group justify="flex-end" gap="xs">
                  <Button variant="default" onClick={() => void execute(state.ctx, state.prepared, false)}>
                    {t("webSync.deletes.skip")}
                  </Button>
                  <Button color="red" disabled={state.prepared.summary.mass && !massChecked} onClick={() => void execute(state.ctx, state.prepared, true)}>
                    {t("webSync.deletes.apply")}
                  </Button>
                </Group>
              </Stack>
            </Alert>
          </Stack>
        )}

        {state.kind === "running" && (
          <Stack gap={4}>
            <Progress
              value={state.progress.totalBytes > 0 ? (100 * state.progress.bytes) / state.progress.totalBytes : state.progress.total > 0 ? (100 * state.progress.done) / state.progress.total : 0}
              animated
              aria-label={t("webSync.sync")}
            />
            <Text size="xs" c="dimmed">
              {t("webSync.progress", {
                done: state.progress.done,
                total: state.progress.total,
                bytes: formatBytes(state.progress.bytes),
                totalBytes: formatBytes(state.progress.totalBytes),
              })}
            </Text>
            {state.progress.current && (
              <Text size="xs" c="dimmed" truncate="end">
                {t("webSync.current", { path: state.progress.current })}
              </Text>
            )}
          </Stack>
        )}

        {state.kind === "done" && <OutcomeView outcome={state.outcome} stopped={state.stopped} />}
        {state.kind === "stopped" && (
          <Alert color="yellow">
            <Text size="sm">{t("webSync.result.stopped")}</Text>
          </Alert>
        )}
        {state.kind === "error" && (
          <Alert color="red" icon={<IconAlertTriangle size={18} />}>
            <Text size="sm">{state.message}</Text>
          </Alert>
        )}

        <Group justify="space-between" wrap="nowrap">
          <Text size="xs" c="dimmed">
            {config?.lastSync ? t("webSync.lastSync", { date: formatDate(config.lastSync) }) : rules.some((r) => r.mode === "include") ? "" : t("webSync.nothingSelected")}
          </Text>
          <Group gap="xs" wrap="nowrap">
            {(state.kind === "preparing" || state.kind === "running") && (
              <Button variant="default" onClick={() => abort.current?.abort()}>
                {t("webSync.stop")}
              </Button>
            )}
            {state.kind !== "confirm" && (
              <Button
                onClick={() => void start()}
                loading={state.kind === "preparing" || state.kind === "running"}
                disabled={!config?.folder || !rules.some((r) => r.mode === "include")}
              >
                {t("webSync.sync")}
              </Button>
            )}
          </Group>
        </Group>
      </Stack>
    </Modal>
  );
}
