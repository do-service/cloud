import { Avatar, Group, Menu, Text, UnstyledButton } from "@mantine/core";
import { notifications } from "@mantine/notifications";
import { IconArrowsExchange, IconLogout, IconSettings } from "@tabler/icons-react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { errorText } from "../api";
import { fetchMe, logout, meQueryKey } from "../auth";
import { t } from "../i18n";
import WebSyncDialog, { webSyncSupported } from "./WebSyncDialog";

// Sign-out, for the header's menu and the phone's (MobileNav).
export function useSignOut() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  return useMutation({
    mutationFn: logout,
    onSuccess: () => {
      // Nothing cached may outlive the session: the next user of this
      // browser must not see the previous one's files.
      queryClient.clear();
      navigate("/login", { replace: true });
    },
    onError: (err) => notifications.show({ color: "red", message: errorText(err) }),
  });
}

// The signed-in user in the header, with sign-out. Renders nothing while
// signed out: the login page has no one to show.
export default function UserMenu() {
  // Shares RequireAuth's query; never fetches on its own once that ran.
  const me = useQuery({ queryKey: meQueryKey, queryFn: ({ signal }) => fetchMe(signal), staleTime: Infinity, retry: false });
  const signOut = useSignOut();
  const [syncing, setSyncing] = useState(false);

  if (!me.data) return null;
  const name = me.data.displayName || me.data.username;
  const supported = webSyncSupported();
  return (
    <>
      <Menu position="bottom-end" withinPortal>
        <Menu.Target>
          <UnstyledButton aria-label={t("user.menu")}>
            <Group gap="xs" wrap="nowrap">
              <Avatar size="sm" radius="xl" name={name} color="initials" />
              <Text size="sm" visibleFrom="sm">
                {name}
              </Text>
            </Group>
          </UnstyledButton>
        </Menu.Target>
        <Menu.Dropdown>
          <Menu.Label>{me.data.username}</Menu.Label>
          <Menu.Item leftSection={<IconSettings size={16} />} component={Link} to="/settings">
            {t("user.settings")}
          </Menu.Item>
          {/* The sync is the account's, not a folder's: it lives next to
              the settings, on every page, rather than in one folder's
              toolbar. */}
          <Menu.Item leftSection={<IconArrowsExchange size={16} />} onClick={() => setSyncing(true)} disabled={!supported}>
            {t("webSync.menu")}
            {!supported && (
              <Text size="xs" c="dimmed" maw={220}>
                {t("webSync.unsupported")}
              </Text>
            )}
          </Menu.Item>
          <Menu.Item leftSection={<IconLogout size={16} />} onClick={() => signOut.mutate()} disabled={signOut.isPending}>
            {t("user.logout")}
          </Menu.Item>
        </Menu.Dropdown>
      </Menu>
      {/* Mounted while signed in, so a pass goes on with the window closed. */}
      {supported && <WebSyncDialog opened={syncing} onClose={() => setSyncing(false)} />}
    </>
  );
}
