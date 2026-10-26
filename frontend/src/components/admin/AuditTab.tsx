import { Autocomplete, Button, Code, Collapse, Group, Pagination, Select, Stack, Table, Text, TextInput } from "@mantine/core";
import { useDebouncedCallback, useDebouncedValue } from "@mantine/hooks";
import { IconFilter } from "@tabler/icons-react";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { useSearchParams } from "react-router-dom";
import { auditQueryKey, fetchAccounts, fetchAudit, type AuditEntry, type AuditQuery } from "../../admin";
import { ApiError } from "../../api";
import { formatDate, formatNumber, locale, t, type MessageKey } from "../../i18n";
import { useIsPhone } from "../../media";
import { ErrorState, PageLoader } from "../States";

// The areas the log groups actions into; each can be picked whole (by
// prefix) or one action at a time.
const FILTERS: { value: string; label: MessageKey }[] = [
  { value: "auth.", label: "admin.audit.filters.auth" },
  { value: "node.", label: "admin.audit.filters.delete" },
  { value: "trash.", label: "admin.audit.filters.trash" },
  { value: "share.", label: "admin.audit.filters.share" },
  { value: "link.", label: "admin.audit.filters.link" },
  { value: "app_password.", label: "admin.audit.filters.appPassword" },
  { value: "device.", label: "admin.audit.filters.device" },
  { value: "admin.", label: "admin.audit.filters.admin" },
];

const ACTIONS: Record<string, MessageKey> = {
  "auth.login": "admin.audit.actions.login",
  "auth.login_failed": "admin.audit.actions.loginFailed",
  "auth.logout": "admin.audit.actions.logout",
  "app_password.create": "admin.audit.actions.appPasswordCreate",
  "app_password.delete": "admin.audit.actions.appPasswordDelete",
  "device.authorize": "admin.audit.actions.deviceAuthorize",
  "device.revoke": "admin.audit.actions.deviceRevoke",
  "device.signout": "admin.audit.actions.deviceSignout",
  "node.trash": "admin.audit.actions.nodeTrash",
  "trash.purge": "admin.audit.actions.trashPurge",
  "trash.empty": "admin.audit.actions.trashEmpty",
  "share.grant": "admin.audit.actions.shareGrant",
  "share.update": "admin.audit.actions.shareUpdate",
  "share.revoke": "admin.audit.actions.shareRevoke",
  "link.create": "admin.audit.actions.linkCreate",
  "link.update": "admin.audit.actions.linkUpdate",
  "link.delete": "admin.audit.actions.linkDelete",
  "admin.user.create": "admin.audit.actions.userCreate",
  "admin.user.update": "admin.audit.actions.userUpdate",
  "admin.user.delete": "admin.audit.actions.userDelete",
  "admin.group.create": "admin.audit.actions.groupCreate",
  "admin.group.rename": "admin.audit.actions.groupRename",
  "admin.group.delete": "admin.audit.actions.groupDelete",
  "admin.group.member_add": "admin.audit.actions.groupMemberAdd",
  "admin.group.member_remove": "admin.audit.actions.groupMemberRemove",
};

function actionText(action: string): string {
  const key = ACTIONS[action];
  return key ? t(key) : action;
}

function details(e: AuditEntry): string {
  const entries = Object.entries(e.details);
  return entries.length === 0 ? "" : entries.map(([k, v]) => `${k}: ${typeof v === "string" ? v : JSON.stringify(v)}`).join(", ");
}

const AUDIT_SIZES = [25, 50, 100, 200] as const;
const DEFAULT_SIZE = 50;
const TEXT_DELAY_MS = 300;

// The tab's state as it is in the URL, so a reload or a shared link shows
// the same page. from and to are local dates (YYYY-MM-DD), both inclusive.
type AuditView = {
  page: number;
  size: number;
  action: string;
  actor: string;
  from: string;
  to: string;
  q: string;
  ip: string;
};

type FilterKey = Exclude<keyof AuditView, "page" | "size">;
type TextKey = "actor" | "q" | "ip";

const FILTER_KEYS: readonly FilterKey[] = ["action", "actor", "from", "to", "q", "ip"];

function isAction(v: string): boolean {
  return v in ACTIONS || FILTERS.some((f) => f.value === v);
}

function isDate(v: string): boolean {
  return /^\d{4}-\d{2}-\d{2}$/.test(v) && !Number.isNaN(new Date(`${v}T00:00:00`).getTime());
}

// readView takes what it can use from the URL; anything else is the default.
function readView(params: URLSearchParams): AuditView {
  const page = Number(params.get("page"));
  const size = Number(params.get("size"));
  const action = params.get("action") ?? "";
  const from = params.get("from") ?? "";
  const to = params.get("to") ?? "";
  return {
    page: Number.isInteger(page) && page >= 1 ? page : 1,
    size: AUDIT_SIZES.some((s) => s === size) ? size : DEFAULT_SIZE,
    action: isAction(action) ? action : "",
    actor: params.get("actor") ?? "",
    from: isDate(from) ? from : "",
    to: isDate(to) ? to : "",
    q: params.get("q") ?? "",
    ip: params.get("ip") ?? "",
  };
}

// writeView applies change to the URL's params, leaving defaults out. Any
// change but the page itself goes back to the first page: the old one may
// not exist any more.
function writeView(params: URLSearchParams, change: Partial<AuditView>): URLSearchParams {
  const next = new URLSearchParams(params);
  for (const key of FILTER_KEYS) {
    const v = change[key];
    if (v === undefined) continue;
    if (v) next.set(key, v);
    else next.delete(key);
  }
  if (change.size !== undefined) {
    if (change.size === DEFAULT_SIZE) next.delete("size");
    else next.set("size", String(change.size));
  }
  if (change.page !== undefined && change.page > 1) next.set("page", String(change.page));
  else next.delete("page");
  return next;
}

const pad = (n: number) => String(n).padStart(2, "0");

// localMidnight is the start of the local day date plus days, in RFC 3339
// with the browser's offset: the server doesn't know the admin's zone.
function localMidnight(date: string, days = 0): string {
  const [y, m, d] = date.split("-").map(Number);
  const at = new Date(y, m - 1, d + days);
  const offset = -at.getTimezoneOffset();
  const sign = offset < 0 ? "-" : "+";
  const abs = Math.abs(offset);
  return (
    `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}T00:00:00` +
    `${sign}${pad(Math.floor(abs / 60))}:${pad(abs % 60)}`
  );
}

function toQuery(v: AuditView): AuditQuery {
  return {
    ...v,
    from: v.from && localMidnight(v.from),
    // "По" includes the whole day: up to the next midnight.
    to: v.to && localMidnight(v.to, 1),
  };
}

const plurals = new Intl.PluralRules(locale);

function totalText(count: number): string {
  const form = plurals.select(count);
  const key: MessageKey = form === "one" ? "admin.audit.total.one" : form === "few" ? "admin.audit.total.few" : "admin.audit.total.many";
  return t(key, { count: formatNumber(count) });
}

// One option for the whole log, then each area: all of it, then its actions.
const actionOptions = () => [
  { value: "", label: t("admin.audit.filters.all") },
  ...FILTERS.map((f) => ({
    group: t(f.label),
    items: [
      { value: f.value, label: t("admin.audit.groupAll", { group: t(f.label) }) },
      ...Object.entries(ACTIONS)
        .filter(([action]) => action.startsWith(f.value))
        .map(([action, label]) => ({ value: action, label: t(label) })),
    ],
  })),
];

// The audit log (R19), newest first, a page at a time.
export default function AuditTab() {
  const [params, setParams] = useSearchParams();
  const view = readView(params);
  const update = (change: Partial<AuditView>) => setParams((prev) => writeView(prev, change), { replace: true });
  const phone = useIsPhone();
  // On a phone the filters stack, each the full width.
  const fieldWidth = (w: number) => (phone ? "100%" : w);

  // Typed filters go to the URL (and the server) once typing pauses. All
  // of them at once: typing in another field restarts the wait.
  const [text, setText] = useState<Record<TextKey, string>>({ actor: view.actor, q: view.q, ip: view.ip });
  const commitText = useDebouncedCallback(
    (typed: Record<TextKey, string>) => update({ actor: typed.actor.trim(), q: typed.q.trim(), ip: typed.ip.trim() }),
    TEXT_DELAY_MS,
  );
  const typeText = (key: TextKey, value: string) => {
    const typed = { ...text, [key]: value };
    setText(typed);
    commitText(typed);
  };

  const [actorSearch] = useDebouncedValue(text.actor.trim(), TEXT_DELAY_MS);
  const suggestions = useQuery({
    queryKey: ["admin", "users", "suggest", actorSearch],
    queryFn: ({ signal }) => fetchAccounts(actorSearch, null, signal),
    enabled: actorSearch.length > 0,
  });

  const query = toQuery(view);
  const log = useQuery({
    queryKey: [...auditQueryKey, query],
    queryFn: ({ signal }) => fetchAudit(query, signal),
    // The table stays while the next page loads instead of blinking.
    placeholderData: keepPreviousData,
  });
  // The only thing the server can refuse in a query built here is the IP.
  const badIp = view.ip !== "" && log.error instanceof ApiError && log.error.code === "invalid_request";

  const filtered = FILTER_KEYS.some((k) => view[k] !== "") || text.actor !== "" || text.q !== "" || text.ip !== "";
  // On a phone the six fields, stacked, would fill the first screen: they
  // fold behind a button that counts the ones in use.
  const [filtersOpen, setFiltersOpen] = useState(false);
  const inUse = FILTER_KEYS.filter((k) => view[k] !== "").length;
  const reset = () => {
    commitText.cancel();
    setText({ actor: "", q: "", ip: "" });
    update({ action: "", actor: "", from: "", to: "", q: "", ip: "" });
  };

  const items = log.data?.items ?? [];
  const pages = log.data ? Math.ceil(log.data.total / log.data.size) : 0;
  const resetButton = filtered && (
    <Button variant="subtle" onClick={reset}>
      {t("admin.audit.reset")}
    </Button>
  );
  const fields = (
    <Group gap="sm" align="flex-end" wrap="wrap">
      <Select
        label={t("admin.audit.action")}
        data={actionOptions()}
        value={view.action}
        onChange={(v) => update({ action: v ?? "" })}
        allowDeselect={false}
        searchable
        w={fieldWidth(260)}
      />
      <Autocomplete
        label={t("admin.audit.user")}
        placeholder={t("admin.audit.userPlaceholder")}
        data={[...new Set((suggestions.data?.items ?? []).map((a) => a.username))]}
        value={text.actor}
        onChange={(v) => typeText("actor", v)}
        w={fieldWidth(180)}
      />
      <TextInput type="date" label={t("admin.audit.from")} value={view.from} onChange={(e) => update({ from: e.currentTarget.value })} w={fieldWidth(150)} />
      <TextInput type="date" label={t("admin.audit.to")} value={view.to} onChange={(e) => update({ to: e.currentTarget.value })} w={fieldWidth(150)} />
      <TextInput
        label={t("admin.audit.object")}
        placeholder={t("admin.audit.objectPlaceholder")}
        value={text.q}
        onChange={(e) => typeText("q", e.currentTarget.value)}
        w={fieldWidth(180)}
      />
      <TextInput
        label={t("admin.audit.ip")}
        placeholder={t("admin.audit.ipPlaceholder")}
        value={text.ip}
        onChange={(e) => typeText("ip", e.currentTarget.value)}
        error={badIp ? t("admin.audit.badIp") : undefined}
        w={fieldWidth(180)}
      />
      {!phone && resetButton}
    </Group>
  );
  return (
    <Stack gap="sm">
      {phone ? (
        <>
          <Group gap="sm">
            <Button
              variant="default"
              leftSection={<IconFilter size={16} />}
              onClick={() => setFiltersOpen(!filtersOpen)}
              aria-expanded={filtersOpen}
            >
              {inUse > 0 ? t("admin.audit.filtersInUse", { count: inUse }) : t("admin.audit.filtersButton")}
            </Button>
            {resetButton}
          </Group>
          <Collapse expanded={filtersOpen}>{fields}</Collapse>
        </>
      ) : (
        fields
      )}
      {log.isPending ? (
        <PageLoader />
      ) : badIp ? null : log.data === undefined ? (
        <ErrorState compact error={log.error} onRetry={() => void log.refetch()} />
      ) : items.length === 0 ? (
        <Text c="dimmed" ta="center" py="xl">
          {t("admin.audit.none")}
        </Text>
      ) : (
        <Table verticalSpacing="xs" striped style={{ opacity: log.isPlaceholderData ? 0.6 : undefined }}>
          {/* A phone gets one column: what happened to what, and under it
              when and by whom. */}
          {phone ? (
            <Table.Tbody>
              {items.map((e) => (
                <Table.Tr key={e.id}>
                  <Table.Td>
                    <Text size="sm" style={{ overflowWrap: "anywhere" }}>
                      <Text span inherit fw={500} c={e.action === "auth.login_failed" ? "red" : undefined}>
                        {actionText(e.action)}
                      </Text>
                      {e.target.name ? ` · ${e.target.name}` : e.target.id ? <> · <Code>{e.target.id.slice(0, 8)}</Code></> : ""}
                    </Text>
                    <Text size="xs" c="dimmed">
                      {formatDate(e.at, { dateStyle: "short", timeStyle: "medium" })} · {e.actor.name || "—"}
                    </Text>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          ) : (
            <>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t("admin.audit.when")}</Table.Th>
                  <Table.Th>{t("admin.audit.who")}</Table.Th>
                  <Table.Th>{t("admin.audit.what")}</Table.Th>
                  <Table.Th>{t("admin.audit.object")}</Table.Th>
                  <Table.Th visibleFrom="md">{t("admin.audit.ip")}</Table.Th>
                  <Table.Th visibleFrom="lg">{t("admin.audit.details")}</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {items.map((e) => (
                  <Table.Tr key={e.id}>
                    <Table.Td>
                      <Text size="sm">{formatDate(e.at, { dateStyle: "short", timeStyle: "medium" })}</Text>
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm">{e.actor.name || "—"}</Text>
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm" c={e.action === "auth.login_failed" ? "red" : undefined}>
                        {actionText(e.action)}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm">{e.target.name || (e.target.id ? <Code>{e.target.id.slice(0, 8)}</Code> : "")}</Text>
                    </Table.Td>
                    <Table.Td visibleFrom="md">
                      <Text size="sm" c="dimmed">
                        {e.ip}
                      </Text>
                    </Table.Td>
                    <Table.Td visibleFrom="lg">
                      <Text size="xs" c="dimmed">
                        {details(e)}
                      </Text>
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </>
          )}
        </Table>
      )}
      {log.data && log.data.total > 0 && (
        <Group justify="space-between" wrap="wrap" gap="sm">
          <Text size="sm" c="dimmed">
            {totalText(log.data.total)}
          </Text>
          <Group gap="sm" wrap="wrap">
            <Group gap="xs" wrap="nowrap">
              <Text size="sm">{t("admin.audit.perPage")}</Text>
              <Select
                aria-label={t("admin.audit.perPage")}
                data={AUDIT_SIZES.map(String)}
                value={String(view.size)}
                onChange={(v) => v && update({ size: Number(v) })}
                allowDeselect={false}
                w={90}
              />
            </Group>
            {pages > 1 && (
              <Pagination
                total={pages}
                // The neighbours of the current page don't fit a phone.
                siblings={phone ? 0 : undefined}
                value={view.page}
                onChange={(page) => update({ page })}
                getControlProps={(control) =>
                  control === "previous" ? { "aria-label": t("admin.audit.previous") } : control === "next" ? { "aria-label": t("admin.audit.next") } : {}
                }
              />
            )}
          </Group>
        </Group>
      )}
    </Stack>
  );
}
