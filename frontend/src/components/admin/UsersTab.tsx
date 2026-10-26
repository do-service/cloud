import {
  Alert,
  Badge,
  Button,
  Center,
  Checkbox,
  Group,
  Modal,
  NumberInput,
  PasswordInput,
  Progress,
  Stack,
  Switch,
  Table,
  Text,
  TextInput,
} from "@mantine/core";
import { useDebouncedValue } from "@mantine/hooks";
import { notifications } from "@mantine/notifications";
import { IconPencil, IconSearch, IconUserPlus } from "@tabler/icons-react";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import {
  accountsQueryKey,
  bytesToGiB,
  createAccount,
  deleteAccount,
  fetchAccounts,
  gibToBytes,
  fetchUserDevices,
  revokeUserDevice,
  updateAccount,
  userDevicesQueryKey,
  type Account,
  type AccountChange,
} from "../../admin";
import { errorText } from "../../api";
import { fetchMe, meQueryKey } from "../../auth";
import DeviceList from "../DeviceList";
import { ErrorState, PageLoader } from "../States";
import { formatBytes, formatDate, t } from "../../i18n";
import { useIsPhone } from "../../media";

// Accounts (R15): who there is, how much they use, and the admin's
// changes. Deleting asks for the username typed: it can't be undone.
export default function UsersTab() {
  const queryClient = useQueryClient();
  const phone = useIsPhone();
  const [search, setSearch] = useState("");
  const [query] = useDebouncedValue(search, 300);
  const [editing, setEditing] = useState<Account | null>(null);
  const [creating, setCreating] = useState(false);
  const accounts = useInfiniteQuery({
    queryKey: [...accountsQueryKey, query],
    queryFn: ({ pageParam, signal }) => fetchAccounts(query, pageParam || null, signal),
    initialPageParam: "",
    getNextPageParam: (last) => last.nextCursor ?? undefined,
  });
  const items = accounts.data?.pages.flatMap((p) => p.items) ?? [];
  // A failed next page keeps the rows above it; its retry replaces "more".
  const moreFailed = accounts.isFetchNextPageError && !accounts.isFetchingNextPage;
  const refresh = () => void queryClient.invalidateQueries({ queryKey: accountsQueryKey });

  return (
    <Stack gap="sm">
      <Group justify="space-between">
        <TextInput
          placeholder={t("admin.users.search")}
          aria-label={t("admin.users.search")}
          leftSection={<IconSearch size={16} />}
          value={search}
          onChange={(e) => setSearch(e.currentTarget.value)}
          w={phone ? "100%" : 320}
        />
        <Button size="xs" leftSection={<IconUserPlus size={16} />} onClick={() => setCreating(true)}>
          {t("admin.users.create")}
        </Button>
      </Group>
      {accounts.isPending ? (
        <PageLoader />
      ) : accounts.data === undefined ? (
        <ErrorState compact error={accounts.error} onRetry={() => void accounts.refetch()} />
      ) : items.length === 0 ? (
        <Text c="dimmed" ta="center" py="xl">
          {t("admin.users.none")}
        </Text>
      ) : (
        <Table highlightOnHover verticalSpacing="xs">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>{t("admin.users.user")}</Table.Th>
              {/* A phone has room for one column: usage and status go
                  under the name instead. */}
              {!phone && <Table.Th w={220}>{t("admin.users.space")}</Table.Th>}
              {!phone && <Table.Th>{t("admin.users.status")}</Table.Th>}
              <Table.Th visibleFrom="md">{t("admin.users.lastSeen")}</Table.Th>
              <Table.Th w={40} />
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {items.map((a) => (
              <Table.Tr key={a.id}>
                <Table.Td>
                  <Text size="sm" fw={500}>
                    {a.displayName}
                  </Text>
                  <Text size="xs" c="dimmed" style={phone ? { overflowWrap: "anywhere" } : undefined}>
                    {a.username}
                    {a.email ? ` · ${a.email}` : ""}
                  </Text>
                  {phone && (
                    <Stack gap={4} mt={4}>
                      <Usage account={a} />
                      <Badges account={a} />
                    </Stack>
                  )}
                </Table.Td>
                {!phone && (
                  <Table.Td>
                    <Usage account={a} />
                  </Table.Td>
                )}
                {!phone && (
                  <Table.Td>
                    <Badges account={a} />
                  </Table.Td>
                )}
                <Table.Td visibleFrom="md">
                  <Text size="sm" c={a.lastSeenAt ? undefined : "dimmed"}>
                    {a.lastSeenAt ? formatDate(a.lastSeenAt) : t("admin.users.never")}
                  </Text>
                </Table.Td>
                <Table.Td>
                  {a.status !== "deleting" && (
                    <Button
                      variant="subtle"
                      size="compact-sm"
                      color="gray"
                      onClick={() => setEditing(a)}
                      aria-label={t("admin.users.editNamed", { name: a.username })}
                    >
                      <IconPencil size={16} />
                    </Button>
                  )}
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      )}
      {moreFailed && <ErrorState compact error={accounts.error} onRetry={() => void accounts.fetchNextPage()} />}
      {accounts.hasNextPage && !moreFailed && (
        <Center>
          <Button variant="default" size="xs" loading={accounts.isFetchingNextPage} onClick={() => void accounts.fetchNextPage()}>
            {t("trash.more")}
          </Button>
        </Center>
      )}
      <CreateDialog opened={creating} onClose={() => setCreating(false)} onDone={refresh} />
      {editing && <EditDialog account={editing} onClose={() => setEditing(null)} onDone={refresh} />}
    </Stack>
  );
}

function Badges({ account: a }: { account: Account }) {
  return (
    <Group gap={4}>
      <StatusBadge status={a.status} />
      {a.isAdmin && (
        <Badge variant="light" color="violet" size="sm">
          {t("admin.users.admin")}
        </Badge>
      )}
    </Group>
  );
}

function StatusBadge({ status }: { status: Account["status"] }) {
  const look = { active: "green", disabled: "gray", deleting: "red" } as const;
  return (
    <Badge variant="light" color={look[status]} size="sm">
      {t(`admin.users.statuses.${status}`)}
    </Badge>
  );
}

function Usage({ account }: { account: Account }) {
  if (account.quotaBytes === null) {
    return <Text size="sm">{t("admin.users.usedUnlimited", { used: formatBytes(account.usedBytes) })}</Text>;
  }
  const share = account.quotaBytes > 0 ? (account.usedBytes / account.quotaBytes) * 100 : 100;
  return (
    <Stack gap={2}>
      <Text size="sm">{t("admin.users.usedOf", { used: formatBytes(account.usedBytes), total: formatBytes(account.quotaBytes) })}</Text>
      <Progress size="xs" value={Math.min(share, 100)} color={share >= 90 ? "red" : undefined} aria-hidden />
    </Stack>
  );
}

// QuotaField edits a quota in GiB; null is no limit.
function QuotaField({ value, onChange }: { value: number | null; onChange: (v: number | null) => void }) {
  return (
    <Group align="flex-end" gap="sm">
      <NumberInput
        label={t("admin.users.quota")}
        suffix={` ${t("units.bytes.gb")}`}
        min={0}
        decimalScale={2}
        disabled={value === null}
        value={value === null ? "" : bytesToGiB(value)}
        onChange={(v) => onChange(typeof v === "number" ? gibToBytes(v) : 0)}
        w={180}
      />
      <Checkbox
        label={t("admin.users.unlimited")}
        checked={value === null}
        onChange={(e) => onChange(e.currentTarget.checked ? null : gibToBytes(10))}
        mb={8}
      />
    </Group>
  );
}

function CreateDialog({ opened, onClose, onDone }: { opened: boolean; onClose: () => void; onDone: () => void }) {
  const [username, setUsername] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [password, setPassword] = useState("");
  const [isAdmin, setIsAdmin] = useState(false);
  const [quota, setQuota] = useState<number | null>(gibToBytes(10));
  const phone = useIsPhone();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const close = () => {
    setUsername("");
    setDisplayName("");
    setPassword("");
    setIsAdmin(false);
    setError(null);
    onClose();
  };
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      await createAccount({ username: username.trim(), displayName: displayName.trim(), password, isAdmin, quotaBytes: quota });
      notifications.show({ message: t("admin.users.created", { name: username.trim() }) });
      onDone();
      close();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  };
  return (
    <Modal opened={opened} onClose={close} title={t("admin.users.create")} fullScreen={phone}>
      <form onSubmit={(e) => void submit(e)}>
        <Stack>
          <TextInput label={t("login.username")} value={username} onChange={(e) => setUsername(e.currentTarget.value)} required autoFocus />
          <TextInput label={t("admin.users.displayName")} value={displayName} onChange={(e) => setDisplayName(e.currentTarget.value)} />
          <PasswordInput
            label={t("login.password")}
            description={t("admin.users.passwordHint")}
            value={password}
            onChange={(e) => setPassword(e.currentTarget.value)}
            required
            autoComplete="new-password"
          />
          <QuotaField value={quota} onChange={setQuota} />
          <Switch label={t("admin.users.admin")} checked={isAdmin} onChange={(e) => setIsAdmin(e.currentTarget.checked)} />
          {error !== null && <Alert color="red">{errorText(error)}</Alert>}
          <Group justify="flex-end">
            <Button variant="default" onClick={close}>
              {t("actions.cancel")}
            </Button>
            <Button type="submit" loading={busy}>
              {t("actions.create")}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

function EditDialog({ account, onClose, onDone }: { account: Account; onClose: () => void; onDone: () => void }) {
  const me = useQuery({ queryKey: meQueryKey, queryFn: ({ signal }) => fetchMe(signal), staleTime: Infinity, retry: false });
  const self = me.data?.id === account.id;
  const phone = useIsPhone();
  const [displayName, setDisplayName] = useState(account.displayName);
  const [quota, setQuota] = useState(account.quotaBytes);
  const [isAdmin, setIsAdmin] = useState(account.isAdmin);
  const [disabled, setDisabled] = useState(account.status === "disabled");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [typed, setTyped] = useState("");

  const run = async (action: () => Promise<unknown>, done: string) => {
    setBusy(true);
    setError(null);
    try {
      await action();
      notifications.show({ message: done });
      onDone();
      onClose();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  };
  const save = (e: FormEvent) => {
    e.preventDefault();
    // Only what changed: each change is audited as made.
    const change: AccountChange = {};
    if (displayName.trim() !== account.displayName) change.displayName = displayName.trim();
    if (quota !== account.quotaBytes) change.quotaBytes = quota;
    if (isAdmin !== account.isAdmin) change.isAdmin = isAdmin;
    if (disabled !== (account.status === "disabled")) change.status = disabled ? "disabled" : "active";
    if (password) change.password = password;
    if (Object.keys(change).length === 0) {
      onClose();
      return;
    }
    void run(() => updateAccount(account.id, change), t("admin.users.saved"));
  };

  if (confirmDelete) {
    return (
      <Modal opened onClose={onClose} title={t("admin.users.deleteTitle", { name: account.username })} fullScreen={phone}>
        <Stack>
          <Alert color="red">{t("admin.users.deleteWarning", { used: formatBytes(account.usedBytes) })}</Alert>
          <TextInput
            label={t("admin.users.deleteType", { name: account.username })}
            value={typed}
            onChange={(e) => setTyped(e.currentTarget.value)}
            autoFocus
          />
          {error !== null && <Alert color="red">{errorText(error)}</Alert>}
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setConfirmDelete(false)}>
              {t("actions.cancel")}
            </Button>
            <Button
              color="red"
              loading={busy}
              disabled={typed !== account.username}
              onClick={() => void run(() => deleteAccount(account.id), t("admin.users.deleted", { name: account.username }))}
            >
              {t("admin.users.delete")}
            </Button>
          </Group>
        </Stack>
      </Modal>
    );
  }
  return (
    <Modal opened onClose={onClose} title={t("admin.users.editTitle", { name: account.username })} fullScreen={phone}>
      <form onSubmit={save}>
        <Stack>
          <TextInput label={t("admin.users.displayName")} value={displayName} onChange={(e) => setDisplayName(e.currentTarget.value)} required />
          <QuotaField value={quota} onChange={setQuota} />
          <Switch
            label={t("admin.users.admin")}
            checked={isAdmin}
            disabled={self}
            description={self ? t("admin.users.selfHint") : undefined}
            onChange={(e) => setIsAdmin(e.currentTarget.checked)}
          />
          <Switch
            label={t("admin.users.blocked")}
            description={t("admin.users.blockedHint")}
            checked={disabled}
            disabled={self}
            onChange={(e) => setDisabled(e.currentTarget.checked)}
          />
          <PasswordInput
            label={t("admin.users.newPassword")}
            description={account.hasPassword ? t("admin.users.newPasswordHint") : t("admin.users.setPasswordHint")}
            value={password}
            onChange={(e) => setPassword(e.currentTarget.value)}
            autoComplete="new-password"
          />
          {account.providers.length > 0 && (
            <Text size="xs" c="dimmed">
              {t("admin.users.providers", { list: account.providers.join(", ") })}
            </Text>
          )}
          <Stack gap={4}>
            <Text size="sm" fw={500}>
              {t("devices.title")}
            </Text>
            <Text size="xs" c="dimmed">
              {t("devices.adminHint")}
            </Text>
            <DeviceList
              queryKey={userDevicesQueryKey(account.id)}
              fetch={(signal) => fetchUserDevices(account.id, signal)}
              revoke={(id) => revokeUserDevice(account.id, id)}
            />
          </Stack>
          {error !== null && <Alert color="red">{errorText(error)}</Alert>}
          <Group justify="space-between">
            <Button variant="subtle" color="red" disabled={self} onClick={() => setConfirmDelete(true)}>
              {t("admin.users.delete")}
            </Button>
            <Group gap="xs">
              <Button variant="default" onClick={onClose}>
                {t("actions.cancel")}
              </Button>
              <Button type="submit" loading={busy}>
                {t("admin.users.save")}
              </Button>
            </Group>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}
