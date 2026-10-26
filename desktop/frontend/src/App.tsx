import { Alert, Badge, Box, Button, Center, Group, Menu, Modal, NavLink, ActionIcon, ScrollArea, Stack, Text, Title, Tooltip } from "@mantine/core";
import { useMediaQuery } from "@mantine/hooks";
import { IconDots, IconFolderOpen, IconFolderShare, IconPlayerPause, IconPlayerPlay, IconPlus, IconUnlink } from "@tabler/icons-react";
import { QueryClientProvider, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useState } from "react";
import { ApiError } from "@web/api";
import { ErrorState, PageLoader } from "@web/components/States";
import { t } from "@web/i18n";
import { createQueryClient } from "@web/queryClient";
import AddCloud from "./AddCloud";
import {
  adoptableQueryKey,
  adoptExisting,
  appErrorText,
  connectionsQueryKey,
  fetchAdoptable,
  fetchConnections,
  openFolder,
  removeConnection,
  setActive,
  setPaused,
  type Connection,
  type Status,
} from "./appApi";
import Browser from "./Browser";
import MoveFolder from "./MoveFolder";

const STATUS_COLOR: Record<Status, string> = {
  starting: "gray",
  synced: "teal",
  syncing: "blue",
  paused: "gray",
  offline: "orange",
  error: "red",
  needs_signin: "red",
  no_folder: "red",
  busy: "yellow",
  moving: "blue",
};

function name(c: Connection): string {
  return `${c.user}@${c.host}`;
}

// The window (docs/desktop-client.md, «Интерфейс»): the connections on the
// left with «Добавить облако», the chosen one's cloud on the right.
export default function App() {
  const queryClient = useQueryClient();
  const conns = useQuery({ queryKey: connectionsQueryKey, queryFn: fetchConnections, refetchInterval: 1500 });
  const [adding, setAdding] = useState(false);
  // The connection shown is the app's (active): its /api goes there, so
  // it is switched in the app first and shown once the list says so.
  const show = useCallback(
    async (id: string) => {
      await setActive(id);
      await queryClient.invalidateQueries({ queryKey: connectionsQueryKey });
    },
    [queryClient],
  );
  const added = useCallback(
    (id: string) => {
      setAdding(false);
      void show(id);
    },
    [show],
  );

  if (conns.data === undefined) {
    return conns.error ? <ErrorState error={conns.error} onRetry={() => void conns.refetch()} /> : <PageLoader />;
  }
  const items = conns.data.items;
  const current = items.find((c) => c.id === conns.data.active);

  return (
    <Group h="100dvh" gap={0} align="stretch" wrap="nowrap">
      <Stack w={260} p="sm" gap="xs" style={{ borderRight: "1px solid var(--mantine-color-default-border)", flexShrink: 0 }}>
        <Text size="xs" fw={600} c="dimmed" tt="uppercase" px="xs">
          {t("desktop.connections")}
        </Text>
        <ScrollArea flex={1}>
          {items.map((c) => (
            <NavLink
              key={c.id}
              active={c.id === current?.id}
              label={<Text size="sm" truncate="end">{name(c)}</Text>}
              description={t(`desktop.status.${c.status}`)}
              leftSection={<Box w={8} h={8} style={{ borderRadius: "50%", background: `var(--mantine-color-${STATUS_COLOR[c.status]}-6)` }} />}
              onClick={() => void show(c.id)}
            />
          ))}
        </ScrollArea>
        <Button variant="light" leftSection={<IconPlus size={16} />} onClick={() => setAdding(true)}>
          {t("desktop.addCloud")}
        </Button>
      </Stack>
      <Box flex={1} miw={0} p="md">
        {current ? (
          <ConnectionView key={current.id} conn={current} />
        ) : (
          <Center h="100%">
            <Stack align="center" maw={420} gap="sm">
              <Title order={3}>{t("desktop.empty.title")}</Title>
              <Text c="dimmed" ta="center">
                {t("desktop.empty.text")}
              </Text>
              <Button leftSection={<IconPlus size={16} />} onClick={() => setAdding(true)}>
                {t("desktop.addCloud")}
              </Button>
            </Stack>
          </Center>
        )}
      </Box>
      <AddCloud opened={adding} onClose={() => setAdding(false)} onAdded={added} />
    </Group>
  );
}

const HINTS = ["needs_signin", "no_folder", "busy"] as const;

// Below this window width the header's buttons lose their labels (kept in
// tooltips), so the connection's name stays readable beside them.
const COMPACT_MEDIA = "(max-width: 74.99em)";

function ConnectionView({ conn }: { conn: Connection }) {
  const queryClient = useQueryClient();
  // Its own cache: the web code's query keys don't name the server, and
  // another connection's folder of the same id is another folder.
  const [browserClient] = useState(() => createQueryClient());
  const [removing, setRemoving] = useState(false);
  const [moving, setMoving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const paused = conn.status === "paused";
  const refresh = () => void queryClient.invalidateQueries({ queryKey: connectionsQueryKey });
  const pause = useMutation({
    mutationFn: () => setPaused(conn.id, !paused),
    onError: (err) => setError(appErrorText(err)),
    onSettled: refresh,
  });
  const hint = HINTS.find((h) => h === conn.status);
  // Folders the sync folder held when connected and the cloud has too,
  // left unchecked: offered to check at once.
  const ready = conn.status !== "starting" && conn.status !== "moving" && conn.status !== "busy";
  const adoptable = useQuery({
    queryKey: adoptableQueryKey(conn.id),
    queryFn: () => fetchAdoptable(conn.id),
    enabled: ready,
    refetchInterval: 5000,
    retry: false,
  });
  const adopt = useMutation({
    mutationFn: () => adoptExisting(conn.id),
    onError: (err) => setError(appErrorText(err)),
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: adoptableQueryKey(conn.id) });
      void browserClient.invalidateQueries();
      refresh();
    },
  });
  const adoptCount = adoptable.data?.length ?? 0;
  const compact = useMediaQuery(COMPACT_MEDIA, false, { getInitialValueInEffect: false });
  const openLocal = () => openFolder(conn.id).catch((err: unknown) => setError(appErrorText(err)));
  const pauseLabel = paused ? t("desktop.resume") : t("desktop.pause");
  const pauseIcon = paused ? <IconPlayerPlay size={16} /> : <IconPlayerPause size={16} />;

  return (
    <Stack h="100%" gap="sm">
      <Group justify="space-between" wrap="nowrap" align="flex-start">
        <Box miw={0}>
          <Group gap="xs" wrap="nowrap">
            <Title order={3} lineClamp={1}>
              {name(conn)}
            </Title>
            <Badge color={STATUS_COLOR[conn.status]} variant="light" style={{ flexShrink: 0 }}>
              {t(`desktop.status.${conn.status}`)}
            </Badge>
            {conn.pending > 0 && (
              <Text size="sm" c="dimmed" style={{ flexShrink: 0 }}>
                {t("desktop.pending", { count: conn.pending })}
              </Text>
            )}
          </Group>
          {/* Cut from the start: the folder's own name is the telling end.
              The mark keeps the text's order in truncate's RTL box. */}
          <Text size="sm" c="dimmed" truncate="start" title={conn.folder}>
            {`\u200E${t("desktop.folder", { path: conn.folder })}`}
          </Text>
        </Box>
        <Group gap="xs" wrap="nowrap" style={{ flexShrink: 0 }}>
          {compact ? (
            <>
              <Tooltip label={t("desktop.openFolder")}>
                <ActionIcon variant="default" size="lg" aria-label={t("desktop.openFolder")} onClick={() => void openLocal()}>
                  <IconFolderOpen size={16} />
                </ActionIcon>
              </Tooltip>
              <Tooltip label={pauseLabel}>
                <ActionIcon variant="default" size="lg" aria-label={pauseLabel} loading={pause.isPending} onClick={() => pause.mutate()}>
                  {pauseIcon}
                </ActionIcon>
              </Tooltip>
            </>
          ) : (
            <>
              <Button variant="default" size="sm" leftSection={<IconFolderOpen size={16} />} onClick={() => void openLocal()}>
                {t("desktop.openFolder")}
              </Button>
              <Button variant="default" size="sm" leftSection={pauseIcon} loading={pause.isPending} onClick={() => pause.mutate()}>
                {pauseLabel}
              </Button>
            </>
          )}
          <Menu position="bottom-end">
            <Menu.Target>
              <ActionIcon variant="default" size="lg" aria-label={t("desktop.actions")}>
                <IconDots size={16} />
              </ActionIcon>
            </Menu.Target>
            <Menu.Dropdown>
              <Menu.Item leftSection={<IconFolderShare size={16} />} disabled={conn.status === "moving"} onClick={() => setMoving(true)}>
                {t("desktop.changeFolder")}
              </Menu.Item>
              <Menu.Item color="red" leftSection={<IconUnlink size={16} />} onClick={() => setRemoving(true)}>
                {t("desktop.remove")}
              </Menu.Item>
            </Menu.Dropdown>
          </Menu>
        </Group>
      </Group>
      {hint && (
        <Alert color={STATUS_COLOR[conn.status]} py="xs">
          {t(`desktop.statusHint.${hint}`)}
        </Alert>
      )}
      {adoptCount > 0 && (
        <Alert color="blue" py="xs">
          <Group justify="space-between" wrap="nowrap" gap="sm">
            <Text size="sm">{t("desktop.adopt.text", { count: adoptCount })}</Text>
            <Button size="xs" variant="light" loading={adopt.isPending} onClick={() => adopt.mutate()} style={{ flexShrink: 0 }}>
              {t("desktop.adopt.action")}
            </Button>
          </Group>
        </Alert>
      )}
      {error && (
        <Alert color="red" withCloseButton onClose={() => setError(null)} py="xs">
          {error}
        </Alert>
      )}
      <Box flex={1} mih={0}>
        <QueryClientProvider client={browserClient}>
          <Browser connId={conn.id} rootName={conn.host} />
        </QueryClientProvider>
      </Box>
      <RemoveDialog conn={conn} opened={removing} onClose={() => setRemoving(false)} onRemoved={refresh} />
      <MoveFolder conn={conn} title={name(conn)} opened={moving} onClose={() => setMoving(false)} onMoved={refresh} />
    </Stack>
  );
}

function RemoveDialog({ conn, opened, onClose, onRemoved }: { conn: Connection; opened: boolean; onClose: () => void; onRemoved: () => void }) {
  // The server didn't answer: offer to forget the connection here only.
  const [offline, setOffline] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const remove = useMutation({
    mutationFn: (force: boolean) => removeConnection(conn.id, force),
    onSuccess: () => {
      onClose();
      onRemoved();
    },
    onError: (err) => {
      if (err instanceof ApiError && err.code === "network") setOffline(true);
      else setError(appErrorText(err));
    },
  });
  const close = () => {
    setOffline(false);
    setError(null);
    onClose();
  };
  return (
    <Modal opened={opened} onClose={close} title={t("desktop.removeDialog.title", { name: name(conn) })} centered>
      <Stack gap="sm">
        <Text size="sm">{t("desktop.removeDialog.text", { folder: conn.folder })}</Text>
        {offline && (
          <Alert color="orange" py="xs">
            {t("desktop.removeDialog.offline")}
          </Alert>
        )}
        {error && (
          <Alert color="red" py="xs">
            {error}
          </Alert>
        )}
        <Group justify="flex-end">
          <Button variant="default" onClick={close}>
            {t("desktop.cancel")}
          </Button>
          {offline ? (
            <Button color="red" loading={remove.isPending} onClick={() => remove.mutate(true)}>
              {t("desktop.removeDialog.force")}
            </Button>
          ) : (
            <Button color="red" loading={remove.isPending} onClick={() => remove.mutate(false)}>
              {t("desktop.removeDialog.confirm")}
            </Button>
          )}
        </Group>
      </Stack>
    </Modal>
  );
}
