import { Button, Group } from "@mantine/core";
import { IconFolder, IconShieldCog, IconTrash, IconUsers } from "@tabler/icons-react";
import { useQuery } from "@tanstack/react-query";
import { Link, useLocation } from "react-router-dom";
import { fetchMe, meQueryKey } from "../auth";
import { t } from "../i18n";

// The sections a signed-in user has, the current one marked; null while
// signed out. Shared with the phone's menu (MobileNav).
export function useSections() {
  const { pathname } = useLocation();
  const me = useQuery({ queryKey: meQueryKey, queryFn: ({ signal }) => fetchMe(signal), staleTime: Infinity, retry: false });
  if (!me.data) return null;
  return [
    { to: "/files", label: t("nav.files"), icon: IconFolder, active: pathname.startsWith("/files") },
    { to: "/shared", label: t("nav.shared"), icon: IconUsers, active: pathname === "/shared" },
    { to: "/trash", label: t("nav.trash"), icon: IconTrash, active: pathname === "/trash" },
    ...(me.data.isAdmin ? [{ to: "/admin", label: t("nav.admin"), icon: IconShieldCog, active: pathname === "/admin" }] : []),
  ];
}

// The sections, for a signed-in user.
export default function HeaderNav() {
  const links = useSections();
  if (!links) return null;
  return (
    <Group gap={4} wrap="nowrap" component="nav" aria-label={t("nav.label")} miw={0} style={{ overflowX: "auto", scrollbarWidth: "none" }}>
      {links.map((l) => (
        <Button
          key={l.to}
          component={Link}
          to={l.to}
          size="xs"
          variant={l.active ? "light" : "subtle"}
          color={l.active ? undefined : "gray"}
          leftSection={<l.icon size={16} />}
          aria-current={l.active ? "page" : undefined}
          // Scrolled, not squeezed, when the header runs out of room.
          style={{ flexShrink: 0 }}
        >
          {l.label}
        </Button>
      ))}
    </Group>
  );
}
