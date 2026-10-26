import { ActionIcon, Badge, Button, CloseButton, Group, Modal, Paper, Select, Stack, Text, TextInput, Title, Tooltip } from "@mantine/core";
import { useDebouncedValue } from "@mantine/hooks";
import { notifications } from "@mantine/notifications";
import { IconPencil, IconTrash, IconUsersPlus } from "@tabler/icons-react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { createGroup, deleteGroup, fetchAccounts, fetchGroups, groupsQueryKey, renameGroup, setMember, type AdminGroup } from "../../admin";
import { errorText } from "../../api";
import { t } from "../../i18n";
import { useIsPhone } from "../../media";
import { ErrorState, PageLoader } from "../States";

// Groups to share with at once (R9): their names and who is in them.
export default function GroupsTab() {
  const queryClient = useQueryClient();
  const groups = useQuery({ queryKey: groupsQueryKey, queryFn: ({ signal }) => fetchGroups(signal) });
  const phone = useIsPhone();
  const [name, setName] = useState("");
  const [renaming, setRenaming] = useState<AdminGroup | null>(null);
  const [removing, setRemoving] = useState<AdminGroup | null>(null);
  const refresh = () => void queryClient.invalidateQueries({ queryKey: groupsQueryKey });

  // Each change says how it went, and the list follows.
  const run = async (action: () => Promise<unknown>, done: string) => {
    try {
      await action();
      notifications.show({ message: done });
      return true;
    } catch (err) {
      notifications.show({ color: "red", message: errorText(err) });
      return false;
    } finally {
      refresh();
    }
  };
  const create = async (e: FormEvent) => {
    e.preventDefault();
    if (await run(() => createGroup(name.trim()), t("admin.groups.created", { name: name.trim() }))) setName("");
  };

  return (
    <Stack gap="sm">
      <form onSubmit={(e) => void create(e)}>
        <Group align="flex-end" gap="xs">
          <TextInput label={t("admin.groups.new")} placeholder={t("admin.groups.newPlaceholder")} value={name} onChange={(e) => setName(e.currentTarget.value)} w={phone ? "100%" : 320} />
          <Button type="submit" leftSection={<IconUsersPlus size={16} />} disabled={!name.trim()}>
            {t("actions.create")}
          </Button>
        </Group>
      </form>
      {groups.isPending ? (
        <PageLoader />
      ) : groups.data === undefined ? (
        <ErrorState compact error={groups.error} onRetry={() => void groups.refetch()} />
      ) : groups.data.length === 0 ? (
        <Text c="dimmed" ta="center" py="xl">
          {t("admin.groups.none")}
        </Text>
      ) : (
        groups.data.map((g) => (
          <Paper key={g.id} withBorder p="sm">
            <Stack gap="xs">
              <Group justify="space-between">
                <Title order={5}>{g.name}</Title>
                <Group gap={4}>
                  <Tooltip label={t("actions.rename")}>
                    <ActionIcon variant="subtle" color="gray" onClick={() => setRenaming(g)} aria-label={t("admin.groups.renameNamed", { name: g.name })}>
                      <IconPencil size={16} />
                    </ActionIcon>
                  </Tooltip>
                  <Tooltip label={t("actions.delete")}>
                    <ActionIcon variant="subtle" color="red" onClick={() => setRemoving(g)} aria-label={t("admin.groups.deleteNamed", { name: g.name })}>
                      <IconTrash size={16} />
                    </ActionIcon>
                  </Tooltip>
                </Group>
              </Group>
              <Group gap={6}>
                {g.members.length === 0 && (
                  <Text size="sm" c="dimmed">
                    {t("admin.groups.empty")}
                  </Text>
                )}
                {g.members.map((m) => (
                  <Badge
                    key={m.id}
                    variant="light"
                    color="gray"
                    tt="none"
                    rightSection={
                      // A real button: Pill's own is hidden from keyboards
                      // and screen readers.
                      <CloseButton
                        size="xs"
                        aria-label={t("admin.groups.removeMember", { name: m.name, group: g.name })}
                        onClick={() => void run(() => setMember(g.id, m.id, false), t("admin.groups.memberRemoved", { name: m.name }))}
                      />
                    }
                  >
                    {m.name}
                  </Badge>
                ))}
              </Group>
              <AddMember group={g} onAdd={(id, who) => void run(() => setMember(g.id, id, true), t("admin.groups.memberAdded", { name: who }))} />
            </Stack>
          </Paper>
        ))
      )}
      {renaming && (
        <RenameDialog
          group={renaming}
          onClose={() => setRenaming(null)}
          onRename={async (newName) => {
            if (await run(() => renameGroup(renaming.id, newName), t("admin.groups.renamed"))) setRenaming(null);
          }}
        />
      )}
      <Modal opened={removing !== null} onClose={() => setRemoving(null)} title={t("admin.groups.deleteTitle", { name: removing?.name ?? "" })} fullScreen={phone}>
        <Stack>
          <Text size="sm">{t("admin.groups.deleteConfirm")}</Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setRemoving(null)}>
              {t("actions.cancel")}
            </Button>
            <Button
              color="red"
              onClick={() => {
                const g = removing;
                setRemoving(null);
                if (g) void run(() => deleteGroup(g.id), t("admin.groups.deleted", { name: g.name }));
              }}
            >
              {t("actions.delete")}
            </Button>
          </Group>
        </Stack>
      </Modal>
    </Stack>
  );
}

// AddMember finds a user by name to add to the group.
function AddMember({ group, onAdd }: { group: AdminGroup; onAdd: (id: string, name: string) => void }) {
  const [search, setSearch] = useState("");
  const [query] = useDebouncedValue(search, 300);
  const found = useQuery({
    queryKey: ["admin", "users", "pick", query],
    queryFn: ({ signal }) => fetchAccounts(query, null, signal),
    enabled: query.trim().length > 0,
  });
  const phone = useIsPhone();
  const members = new Set(group.members.map((m) => m.id));
  const options = (found.data?.items ?? [])
    .filter((a) => !members.has(a.id) && a.status !== "deleting")
    .map((a) => ({ value: a.id, label: `${a.displayName} (${a.username})` }));
  return (
    <Select
      placeholder={t("admin.groups.addMember")}
      aria-label={t("admin.groups.addMemberTo", { group: group.name })}
      searchable
      searchValue={search}
      onSearchChange={setSearch}
      data={options}
      value={null}
      nothingFoundMessage={query ? t("admin.groups.nobody") : undefined}
      onChange={(id) => {
        const picked = options.find((o) => o.value === id);
        if (picked) {
          onAdd(picked.value, picked.label);
          setSearch("");
        }
      }}
      w={phone ? "100%" : 320}
      size="xs"
    />
  );
}

function RenameDialog({ group, onClose, onRename }: { group: AdminGroup; onClose: () => void; onRename: (name: string) => Promise<void> }) {
  const [name, setName] = useState(group.name);
  const phone = useIsPhone();
  return (
    <Modal opened onClose={onClose} title={t("admin.groups.renameTitle", { name: group.name })} fullScreen={phone}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          void onRename(name.trim());
        }}
      >
        <Stack>
          <TextInput label={t("actions.name")} value={name} onChange={(e) => setName(e.currentTarget.value)} required autoFocus />
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              {t("actions.cancel")}
            </Button>
            <Button type="submit" disabled={!name.trim() || name.trim() === group.name}>
              {t("actions.rename")}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}
