import { ActionIcon, Button, Checkbox, Group, Modal, Select, Stack, Tabs, Text, Tooltip } from "@mantine/core";
import { useDebouncedValue } from "@mantine/hooks";
import { notifications } from "@mantine/notifications";
import { IconLink, IconUser, IconUsers, IconUsersGroup, IconX } from "@tabler/icons-react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { errorText } from "../api";
import {
  can,
  fetchShares,
  grantShare,
  revokeShare,
  searchDirectory,
  sharesQueryKey,
  updateShare,
  type FileNode,
  type Grantee,
  type Permission,
  type ShareGrant,
} from "../files";
import { t } from "../i18n";
import { useIsPhone } from "../media";
import LinksPanel from "./LinksPanel";
import { ErrorState, PageLoader } from "./States";

// What the dialog offers: viewing or editing, and whether the recipient may
// share further. Rights map to these levels both ways.
type Level = "view" | "edit";

function toPermissions(level: Level, reshare: boolean): Permission[] {
  const perms: Permission[] = level === "edit" ? ["read", "write"] : ["read"];
  return reshare ? [...perms, "reshare"] : perms;
}

function levelOf(perms: Permission[]): Level {
  return perms.includes("write") ? "edit" : "view";
}

const DIRECTORY_DELAY_MS = 250;

type Props = { node: FileNode | null; onClose: () => void };

// Sharing a file or folder with people and groups (R9): who has it, at
// what level, and adding someone. Nobody grants more than they have; the
// choices are trimmed to the user's own rights.
export default function ShareDialog({ node, onClose }: Props) {
  const phone = useIsPhone();
  const queryClient = useQueryClient();
  const [search, setSearch] = useState("");
  const [debounced] = useDebouncedValue(search, DIRECTORY_DELAY_MS);
  const [picked, setPicked] = useState<Grantee | null>(null);
  const [level, setLevel] = useState<Level>("view");
  const [reshare, setReshare] = useState(false);
  const [busy, setBusy] = useState(false);

  const shares = useQuery({
    queryKey: sharesQueryKey(node?.id ?? ""),
    queryFn: ({ signal }) => fetchShares(node?.id ?? "", signal),
    enabled: node !== null,
  });
  const directory = useQuery({
    queryKey: ["directory", debounced],
    queryFn: ({ signal }) => searchDirectory(debounced, signal),
    enabled: node !== null,
  });

  const canEdit = can(node ?? undefined, "write");
  const existing = new Set((shares.data ?? []).map((s) => `${s.grantee.type}:${s.grantee.id}`));
  const candidates = (directory.data ?? []).filter((g) => !existing.has(`${g.type}:${g.id}`));
  const options = candidates.map((g) => ({
    value: `${g.type}:${g.id}`,
    label: g.type === "group" ? t("share.groupLabel", { name: g.name }) : g.name,
  }));

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: sharesQueryKey(node?.id ?? "") });
  };
  const run = async (action: () => Promise<unknown>, done?: string) => {
    setBusy(true);
    try {
      await action();
      if (done) notifications.show({ message: done });
    } catch (err) {
      notifications.show({ color: "red", message: errorText(err) });
    } finally {
      setBusy(false);
      refresh();
    }
  };

  const add = () => {
    if (!node || !picked) return;
    void run(async () => {
      await grantShare(node.id, picked, toPermissions(level, reshare));
      setPicked(null);
      setSearch("");
    }, t("share.granted", { name: picked.name }));
  };

  const close = () => {
    setPicked(null);
    setSearch("");
    onClose();
  };

  return (
    <Modal opened={node !== null} onClose={close} title={t("share.title", { name: node?.name ?? "" })} size="lg" fullScreen={phone}>
      <Tabs defaultValue="people" keepMounted={false}>
        <Tabs.List mb="md">
          <Tabs.Tab value="people" leftSection={<IconUsers size={16} />}>
            {t("share.tabPeople")}
          </Tabs.Tab>
          <Tabs.Tab value="link" leftSection={<IconLink size={16} />}>
            {t("share.tabLink")}
          </Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="link">{node && <LinksPanel node={node} />}</Tabs.Panel>
        <Tabs.Panel value="people">
          <Stack>
            {/* On a phone the level goes below the search, which then has
                the line to itself and its placeholder fits. */}
            <Group align="flex-end" wrap={phone ? "wrap" : "nowrap"} gap="xs">
              <Select
                style={{ flex: phone ? "1 1 100%" : 1 }}
                label={t("share.with")}
                placeholder={t("share.searchPlaceholder")}
                searchable
                searchValue={search}
                onSearchChange={setSearch}
                data={options}
                // The server matches; showing its answer as is.
                filter={({ options: all }) => all}
                value={picked ? `${picked.type}:${picked.id}` : null}
                onChange={(value) => setPicked(candidates.find((g) => `${g.type}:${g.id}` === value) ?? null)}
                nothingFoundMessage={directory.isFetching ? t("common.loading") : t("share.nobody")}
                clearable
              />
              <Select
                w={170}
                label={t("share.level")}
                data={[
                  { value: "view", label: t("share.view") },
                  { value: "edit", label: t("share.edit"), disabled: !canEdit },
                ]}
                value={level}
                onChange={(v) => setLevel(v === "edit" ? "edit" : "view")}
                allowDeselect={false}
              />
            </Group>
            <Group justify="space-between">
              <Checkbox label={t("share.reshare")} checked={reshare} onChange={(e) => setReshare(e.currentTarget.checked)} />
              <Button onClick={add} disabled={!picked} loading={busy}>
                {t("share.add")}
              </Button>
            </Group>

            <Text fw={600} size="sm" mt="sm">
              {t("share.current")}
            </Text>
            {shares.isPending ? (
              <PageLoader compact />
            ) : shares.data === undefined ? (
              <ErrorState compact error={shares.error} onRetry={() => void shares.refetch()} />
            ) : shares.data.length === 0 ? (
              <Text size="sm" c="dimmed">
                {t("share.none")}
              </Text>
            ) : (
              <Stack gap="xs">
                {shares.data.map((s) => (
                  <ShareRow
                    key={s.id}
                    share={s}
                    canEdit={canEdit}
                    busy={busy}
                    onChange={(perms) => void run(() => updateShare(s.id, perms))}
                    onRevoke={() => void run(() => revokeShare(s.id), t("share.revoked", { name: s.grantee.name }))}
                  />
                ))}
              </Stack>
            )}
          </Stack>
        </Tabs.Panel>
      </Tabs>
    </Modal>
  );
}

function ShareRow({
  share,
  canEdit,
  busy,
  onChange,
  onRevoke,
}: {
  share: ShareGrant;
  canEdit: boolean;
  busy: boolean;
  onChange: (perms: Permission[]) => void;
  onRevoke: () => void;
}) {
  const level = levelOf(share.permissions);
  const reshare = share.permissions.includes("reshare");
  const phone = useIsPhone();
  const iconStyle = { flexShrink: 0 };
  return (
    // A phone puts the controls under the name, which would get no room
    // beside them.
    <Group justify="space-between" wrap={phone ? "wrap" : "nowrap"} gap="xs">
      <Group gap="xs" wrap="nowrap" style={{ minWidth: 0, flex: phone ? "1 1 100%" : undefined }}>
        {share.grantee.type === "group" ? <IconUsersGroup size={18} aria-hidden style={iconStyle} /> : <IconUser size={18} aria-hidden style={iconStyle} />}
        <Text size="sm" truncate="end">
          {share.grantee.name}
        </Text>
      </Group>
      <Group gap="xs" wrap="nowrap">
        <Select
          size="xs"
          w={150}
          aria-label={t("share.levelFor", { name: share.grantee.name })}
          data={[
            { value: "view", label: t("share.view") },
            { value: "edit", label: t("share.edit"), disabled: !canEdit },
          ]}
          value={level}
          disabled={busy}
          onChange={(v) => onChange(toPermissions(v === "edit" ? "edit" : "view", reshare))}
          allowDeselect={false}
        />
        <Checkbox
          size="xs"
          label={t("share.reshareShort")}
          aria-label={t("share.reshareFor", { name: share.grantee.name })}
          checked={reshare}
          disabled={busy}
          onChange={(e) => onChange(toPermissions(level, e.currentTarget.checked))}
          styles={{ label: { whiteSpace: "nowrap" } }}
        />
        <Tooltip label={t("share.revoke")}>
          <ActionIcon variant="subtle" color="gray" disabled={busy} onClick={onRevoke} aria-label={t("share.revokeFor", { name: share.grantee.name })}>
            <IconX size={16} />
          </ActionIcon>
        </Tooltip>
      </Group>
    </Group>
  );
}
