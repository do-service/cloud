import { Center, Stack, Tabs, Text, Title } from "@mantine/core";
import { IconFileText, IconSettings, IconUsers, IconUsersGroup } from "@tabler/icons-react";
import { useQuery } from "@tanstack/react-query";
import { useSearchParams } from "react-router-dom";
import { fetchMe, meQueryKey } from "../auth";
import AuditTab from "../components/admin/AuditTab";
import GroupsTab from "../components/admin/GroupsTab";
import SystemTab from "../components/admin/SystemTab";
import UsersTab from "../components/admin/UsersTab";
import { PageLoader } from "../components/States";
import { t } from "../i18n";
import { useIsPhone } from "../media";

const TABS = ["users", "groups", "audit", "system"] as const;
type Tab = (typeof TABS)[number];

function isTab(v: string | null): v is Tab {
  return TABS.some((tab) => tab === v);
}

// The administrator's pages (E9-11). The tab is in the URL, so a reload or
// a shared link opens the same one. The server refuses everyone else; the
// page only says so.
export default function AdminPage() {
  const me = useQuery({ queryKey: meQueryKey, queryFn: ({ signal }) => fetchMe(signal), staleTime: Infinity, retry: false });
  const [params, setParams] = useSearchParams();
  const raw = params.get("tab");
  const tab: Tab = isTab(raw) ? raw : "users";
  const phone = useIsPhone();

  if (!me.data) return <PageLoader />;
  if (!me.data.isAdmin) {
    return (
      <Center py="xl">
        <Text c="dimmed">{t("admin.forbidden")}</Text>
      </Center>
    );
  }
  return (
    <Stack gap="sm">
      <Title order={3}>{t("admin.title")}</Title>
      <Tabs
        value={tab}
        onChange={(v) => isTab(v) && setParams({ tab: v }, { replace: true })}
        keepMounted={false}
        // On a phone the four tabs share one line, without icons and with
        // less padding, so all of them fit rather than scroll out of sight.
        styles={phone ? { tab: { paddingInline: "var(--mantine-spacing-xs)" } } : undefined}
      >
        <Tabs.List mb="md" grow={phone}>
          <Tabs.Tab value="users" leftSection={!phone && <IconUsers size={16} />}>
            {t("admin.tabs.users")}
          </Tabs.Tab>
          <Tabs.Tab value="groups" leftSection={!phone && <IconUsersGroup size={16} />}>
            {t("admin.tabs.groups")}
          </Tabs.Tab>
          <Tabs.Tab value="audit" leftSection={!phone && <IconFileText size={16} />}>
            {t("admin.tabs.audit")}
          </Tabs.Tab>
          <Tabs.Tab value="system" leftSection={!phone && <IconSettings size={16} />}>
            {t("admin.tabs.system")}
          </Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="users">
          <UsersTab />
        </Tabs.Panel>
        <Tabs.Panel value="groups">
          <GroupsTab />
        </Tabs.Panel>
        <Tabs.Panel value="audit">
          <AuditTab />
        </Tabs.Panel>
        <Tabs.Panel value="system">
          <SystemTab />
        </Tabs.Panel>
      </Tabs>
    </Stack>
  );
}
