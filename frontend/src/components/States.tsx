import { Alert, Button, Center, Group, Loader, Stack, Text, ThemeIcon, Title, type MantineColor } from "@mantine/core";
import { IconAlertTriangle } from "@tabler/icons-react";
import type { ReactNode } from "react";
import { errorText } from "../api";
import { t } from "../i18n";

// The three states every screen with server data goes through besides
// showing it (E9-12), shared so loading, nothing-here and failures look and
// read the same everywhere. compact is for dialogs, tabs and sections, where
// a page-sized block would push the rest of the content away.

export function PageLoader({ compact = false }: { compact?: boolean }) {
  return (
    <Center py={compact ? "sm" : "xl"}>
      <Loader size={compact ? "sm" : "md"} aria-label={t("common.loading")} />
    </Center>
  );
}

type EmptyProps = {
  // An icon at size 36, stroke 1.5, like the rest of the empty states.
  icon: ReactNode;
  title: string;
  description?: string;
  action?: ReactNode;
  color?: MantineColor;
};

export function EmptyState({ icon, title, description, action, color }: EmptyProps) {
  return (
    <Stack align="center" gap="sm" py="xl" ta="center">
      <ThemeIcon size={64} radius="xl" variant="light" color={color}>
        {icon}
      </ThemeIcon>
      <Title order={2}>{title}</Title>
      {description && (
        <Text c="dimmed" maw={420}>
          {description}
        </Text>
      )}
      {action}
    </Stack>
  );
}

type ErrorProps = {
  error: unknown;
  title?: string;
  // Shown as "Повторить"; left out where retrying can't help (not found).
  onRetry?: () => void;
  action?: ReactNode;
  compact?: boolean;
};

export function ErrorState({ error, title, onRetry, action, compact = false }: ErrorProps) {
  const retry = onRetry && (
    <Button variant="light" size={compact ? "xs" : "sm"} color={compact ? "red" : undefined} onClick={onRetry}>
      {t("common.retry")}
    </Button>
  );
  if (compact) {
    return (
      <Alert color="red" title={title}>
        <Stack gap="xs" align="flex-start">
          <Text size="sm">{errorText(error)}</Text>
          {(retry || action) && (
            <Group gap="xs">
              {retry}
              {action}
            </Group>
          )}
        </Stack>
      </Alert>
    );
  }
  return (
    <Stack align="center" gap="sm" py="xl" ta="center" role="alert">
      <ThemeIcon size={64} radius="xl" variant="light" color="red">
        <IconAlertTriangle size={36} stroke={1.5} />
      </ThemeIcon>
      <Title order={2}>{title ?? t("common.errorTitle")}</Title>
      <Text c="dimmed" maw={420}>
        {errorText(error)}
      </Text>
      {(retry || action) && (
        <Group gap="xs" justify="center">
          {retry}
          {action}
        </Group>
      )}
    </Stack>
  );
}
