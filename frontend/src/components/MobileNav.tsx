import { Avatar, Burger, Button, Drawer, Group, NavLink, Stack, Text } from "@mantine/core";
import { IconLogout, IconSettings } from "@tabler/icons-react";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Link, useLocation } from "react-router-dom";
import { fetchMe, meQueryKey } from "../auth";
import { t } from "../i18n";
import BackendStatus from "./BackendStatus";
import { useSections } from "./HeaderNav";
import { useSignOut } from "./UserMenu";

// The phone's header has room for a burger only: it opens the sections,
// settings, the server's state and the user with sign-out. Signed out,
// there's nothing to open, and the server's state shows in its place.
export default function MobileNav() {
  const { pathname } = useLocation();
  const [opened, setOpened] = useState(false);
  const me = useQuery({ queryKey: meQueryKey, queryFn: ({ signal }) => fetchMe(signal), staleTime: Infinity, retry: false });
  const sections = useSections();
  const signOut = useSignOut();
  // Closed by any navigation, a link's in it or Back, so the page gone to
  // is what shows.
  const [shownAt, setShownAt] = useState(pathname);
  if (shownAt !== pathname) {
    setShownAt(pathname);
    setOpened(false);
  }

  if (!me.data || !sections) return <BackendStatus />;
  const name = me.data.displayName || me.data.username;
  const links = [...sections, { to: "/settings", label: t("user.settings"), icon: IconSettings, active: pathname === "/settings" }];
  return (
    <>
      <Burger opened={opened} onClick={() => setOpened(!opened)} size="sm" aria-label={t("nav.menu")} />
      <Drawer opened={opened} onClose={() => setOpened(false)} position="right" size="80%" title={t("app.title")}>
        <Stack gap="md">
          <nav aria-label={t("nav.label")}>
            {links.map((l) => (
              <NavLink
                key={l.to}
                component={Link}
                to={l.to}
                label={l.label}
                leftSection={<l.icon size={18} />}
                active={l.active}
                aria-current={l.active ? "page" : undefined}
                onClick={() => setOpened(false)}
              />
            ))}
          </nav>
          <Group>
            <BackendStatus />
          </Group>
          <Group gap="sm" wrap="nowrap">
            <Avatar size="md" radius="xl" name={name} color="initials" />
            <Stack gap={0} miw={0}>
              <Text size="sm" fw={500} truncate="end">
                {name}
              </Text>
              <Text size="xs" c="dimmed" truncate="end">
                {me.data.username}
              </Text>
            </Stack>
          </Group>
          <Button variant="default" leftSection={<IconLogout size={16} />} onClick={() => signOut.mutate()} loading={signOut.isPending}>
            {t("user.logout")}
          </Button>
        </Stack>
      </Drawer>
    </>
  );
}
