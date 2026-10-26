import { Alert, Button, Center, Group, Paper, Stack, Table, Text, ThemeIcon, Title } from "@mantine/core";
import { IconCheck, IconDeviceDesktop, IconLinkOff, IconX } from "@tabler/icons-react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { useMemo, type ReactNode } from "react";
import { useSearchParams } from "react-router-dom";
import { errorText } from "../api";
import { fetchMe, meQueryKey } from "../auth";
import { authorizeDevice, leaveTo, parseDeviceRequest, type DevicePlatform } from "../devices";
import { t, type MessageKey } from "../i18n";

const PLATFORMS: Record<DevicePlatform, MessageKey> = {
  macos: "deviceAuth.platforms.macos",
  windows: "deviceAuth.platforms.windows",
  linux: "deviceAuth.platforms.linux",
};

// The consent screen of the desktop sign-in (E12-2), behind RequireAuth:
// a signed-out user signs in first and comes back here, query and all.
export default function DeviceAuthorizePage() {
  const [params] = useSearchParams();
  const request = useMemo(() => parseDeviceRequest(params), [params]);
  // RequireAuth has already loaded it.
  const me = useQuery({ queryKey: meQueryKey, queryFn: ({ signal }) => fetchMe(signal), staleTime: Infinity });
  const answer = useMutation({
    mutationFn: async (deny: boolean) => {
      if (!request) throw new Error("no device request");
      const redirect = await authorizeDevice(request, deny);
      // The server builds the address from the same redirect_uri; going
      // anywhere else would hand the code to someone else.
      if (!redirect.startsWith(`${request.redirectUri}?`)) throw new Error("redirect off the app's address");
      return redirect;
    },
    onSuccess: (redirect) => leaveTo(redirect),
  });

  if (!request) {
    return (
      <Card>
        <Result icon={<IconLinkOff size={32} />} color="red" title={t("deviceAuth.invalidTitle")} text={t("deviceAuth.invalidText")} />
      </Card>
    );
  }
  if (answer.isSuccess) {
    const denied = answer.variables;
    return (
      <Card>
        <Result
          icon={denied ? <IconX size={32} /> : <IconCheck size={32} />}
          color={denied ? "gray" : "green"}
          title={t(denied ? "deviceAuth.deniedTitle" : "deviceAuth.allowedTitle")}
          text={t(denied ? "deviceAuth.deniedText" : "deviceAuth.allowedText")}
        />
      </Card>
    );
  }

  const user = me.data;
  return (
    <Card>
      <Stack>
        <Group gap="sm" wrap="nowrap">
          <ThemeIcon size={44} radius="xl" variant="light">
            <IconDeviceDesktop size={26} stroke={1.5} />
          </ThemeIcon>
          <Title order={2}>{t("deviceAuth.title")}</Title>
        </Group>
        <Text>{t("deviceAuth.intro")}</Text>
        <Table variant="vertical" layout="fixed" withTableBorder>
          <Table.Tbody>
            <Row label={t("deviceAuth.device")}>
              <Text fw={500} style={{ overflowWrap: "anywhere" }}>
                {request.name}
              </Text>
              <Text size="sm" c="dimmed">
                {t(PLATFORMS[request.platform])}
                {request.clientVersion && ` · ${t("deviceAuth.version", { version: request.clientVersion })}`}
              </Text>
            </Row>
            {user && (
              <Row label={t("deviceAuth.account")}>
                <Text fw={500} style={{ overflowWrap: "anywhere" }}>
                  {user.displayName}
                </Text>
                <Text size="sm" c="dimmed" style={{ overflowWrap: "anywhere" }}>
                  {user.username}
                </Text>
              </Row>
            )}
            <Row label={t("deviceAuth.server")}>
              <Text fw={500} style={{ overflowWrap: "anywhere" }}>
                {window.location.origin}
              </Text>
            </Row>
          </Table.Tbody>
        </Table>
        <Text size="sm" c="dimmed">
          {t("deviceAuth.warning")}
        </Text>
        {answer.isError && <Alert color="red">{errorText(answer.error)}</Alert>}
        <Group justify="flex-end">
          <Button variant="default" onClick={() => answer.mutate(true)} disabled={answer.isPending}>
            {t("deviceAuth.deny")}
          </Button>
          <Button onClick={() => answer.mutate(false)} loading={answer.isPending && !answer.variables} disabled={answer.isPending}>
            {t("deviceAuth.allow")}
          </Button>
        </Group>
      </Stack>
    </Card>
  );
}

function Card({ children }: { children: ReactNode }) {
  return (
    <Center py="xl">
      <Paper withBorder shadow="sm" p="xl" w="100%" maw={480}>
        {children}
      </Paper>
    </Center>
  );
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Table.Tr>
      <Table.Th w={110}>{label}</Table.Th>
      <Table.Td>{children}</Table.Td>
    </Table.Tr>
  );
}

function Result({ icon, color, title, text }: { icon: ReactNode; color: string; title: string; text: string }) {
  return (
    <Stack align="center" gap="sm" ta="center">
      <ThemeIcon size={64} radius="xl" variant="light" color={color}>
        {icon}
      </ThemeIcon>
      <Title order={2}>{title}</Title>
      <Text c="dimmed">{text}</Text>
    </Stack>
  );
}
