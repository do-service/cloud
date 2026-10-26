import { ActionIcon, Button, Group, Paper, Progress, ScrollArea, Stack, Text, Tooltip } from "@mantine/core";
import { IconChevronDown, IconChevronUp, IconRefresh, IconX } from "@tabler/icons-react";
import { useState } from "react";
import { errorText } from "../api";
import { formatBytes, t } from "../i18n";
import { useUploads, type UploadTask } from "../uploads";
import classes from "./UploadPanel.module.css";

function statusText(task: UploadTask): string {
  switch (task.status) {
    case "queued":
      return t("upload.queued");
    case "uploading":
      return `${formatBytes(task.sent)} / ${formatBytes(task.size)}`;
    case "done":
      return t("upload.done");
    case "canceled":
      return t("upload.canceled");
    case "error":
      return errorText(task.error);
    case "conflict":
      return t("upload.conflict");
  }
}

// Bottom-right card with every upload of the session: progress, cancel,
// retry. Stays across folders, so navigating doesn't hide what's going on.
export default function UploadPanel() {
  const { tasks, cancel, retry, resolve, clearFinished } = useUploads();
  const [collapsed, setCollapsed] = useState(false);
  if (tasks.length === 0) return null;

  const done = tasks.filter((x) => x.status === "done").length;
  const active = tasks.filter((x) => x.status === "uploading" || x.status === "queued");
  const failed = tasks.filter((x) => x.status === "error").length;
  const conflicts = tasks.filter((x) => x.status === "conflict").length;
  const total = active.reduce((sum, x) => sum + x.size, 0);
  const sent = active.reduce((sum, x) => sum + x.sent, 0);

  return (
    <Paper className={classes.panel} shadow="lg" withBorder role="region" aria-label={t("upload.panel")}>
      <Group justify="space-between" wrap="nowrap" px="sm" py="xs" className={classes.header}>
        <Text size="sm" fw={600}>
          {active.length > 0 ? t("upload.progress", { done, count: tasks.length }) : t("upload.finished", { done, count: tasks.length })}
          {failed > 0 && ` · ${t("upload.failedCount", { count: failed })}`}
          {conflicts > 0 && ` · ${t("upload.conflictCount", { count: conflicts })}`}
        </Text>
        <Group gap={4} wrap="nowrap">
          {active.length === 0 && (
            <Button size="compact-xs" variant="subtle" onClick={clearFinished}>
              {t("upload.clear")}
            </Button>
          )}
          <ActionIcon variant="subtle" color="gray" onClick={() => setCollapsed(!collapsed)} aria-label={collapsed ? t("upload.expand") : t("upload.collapse")}>
            {collapsed ? <IconChevronUp size={16} /> : <IconChevronDown size={16} />}
          </ActionIcon>
        </Group>
      </Group>
      {active.length > 0 && <Progress value={total > 0 ? (sent / total) * 100 : 0} size="xs" radius={0} aria-hidden />}
      {conflicts > 1 && (
        <Stack gap={6} px="sm" py="xs" className={classes.header}>
          <Text size="sm">{t("upload.conflicts", { count: conflicts })}</Text>
          <Group gap="xs">
            <Button size="compact-xs" onClick={() => resolve(null, "replace")}>
              {t("upload.replaceAll")}
            </Button>
            <Button size="compact-xs" variant="default" onClick={() => resolve(null, "rename")}>
              {t("upload.keepBothAll")}
            </Button>
            <Button size="compact-xs" variant="subtle" color="gray" onClick={() => resolve(null, "skip")}>
              {t("upload.skipAll")}
            </Button>
          </Group>
        </Stack>
      )}
      {!collapsed && (
        <ScrollArea.Autosize mah={280}>
          <Stack gap={0}>
            {tasks.map((task) => (
              <Group key={task.key} wrap="nowrap" gap="xs" px="sm" py={6} className={classes.row}>
                <Stack gap={2} style={{ flex: 1, minWidth: 0 }}>
                  <Text size="sm" truncate="end" title={task.dir ? `${task.dir}/${task.name}` : task.name}>
                    {task.name}
                  </Text>
                  {task.status === "uploading" && <Progress value={task.size > 0 ? (task.sent / task.size) * 100 : 100} size="xs" />}
                  <Text size="xs" c={task.status === "error" ? "red" : task.status === "conflict" ? "yellow" : "dimmed"} truncate="end">
                    {statusText(task)}
                  </Text>
                  {task.status === "conflict" && (
                    <Group gap={4} mt={2}>
                      <Button size="compact-xs" variant="light" onClick={() => resolve(task.key, "replace")} aria-label={t("upload.replaceName", { name: task.name })}>
                        {t("upload.replace")}
                      </Button>
                      <Button size="compact-xs" variant="default" onClick={() => resolve(task.key, "rename")} aria-label={t("upload.keepBothName", { name: task.name })}>
                        {t("upload.keepBoth")}
                      </Button>
                      <Button size="compact-xs" variant="subtle" color="gray" onClick={() => resolve(task.key, "skip")} aria-label={t("upload.skipName", { name: task.name })}>
                        {t("upload.skip")}
                      </Button>
                    </Group>
                  )}
                </Stack>
                {task.status === "error" && (
                  <Tooltip label={t("upload.retry")}>
                    <ActionIcon variant="subtle" onClick={() => retry(task.key)} aria-label={t("upload.retryName", { name: task.name })}>
                      <IconRefresh size={16} />
                    </ActionIcon>
                  </Tooltip>
                )}
                {(task.status === "uploading" || task.status === "queued") && (
                  <Tooltip label={t("upload.cancel")}>
                    <ActionIcon variant="subtle" color="gray" onClick={() => cancel(task.key)} aria-label={t("upload.cancelName", { name: task.name })}>
                      <IconX size={16} />
                    </ActionIcon>
                  </Tooltip>
                )}
              </Group>
            ))}
          </Stack>
        </ScrollArea.Autosize>
      )}
    </Paper>
  );
}
