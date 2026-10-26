import { Alert, Button, Group, Modal, Progress, Radio, Stack, Text, Title } from "@mantine/core";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { formatDate, formatNumber, t } from "@web/i18n";
import {
  appErrorText,
  checkUpdates,
  downloadUpdate,
  installUpdate,
  openReleasePage,
  setUpdateMode,
  updatesQueryKey,
  UPDATE_MODES,
  type Updates,
} from "./appApi";

function useUpdateAction<T>(fn: () => Promise<T>, onError: (text: string) => void) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: fn,
    onError: (err) => onError(appErrorText(err)),
    onSettled: () => void queryClient.invalidateQueries({ queryKey: updatesQueryKey }),
  });
}

// The banner over the window when a new version is out
// (desktop/internal/update): download it, or, once downloaded and
// verified, install it and restart — only on this click. «Позже» hides it
// until the next version; the tray keeps its item.
export function UpdateBanner({ updates }: { updates: Updates }) {
  const [dismissed, setDismissed] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const download = useUpdateAction(downloadUpdate, setError);
  const install = useUpdateAction(installUpdate, setError);
  const notes = useUpdateAction(openReleasePage, setError);
  const rel = updates.release;
  if (!rel || dismissed === rel.version) return null;
  const { state } = updates;
  const failed = state === "error" && (updates.errorStage === "download" || updates.errorStage === "install");
  if (!failed && state !== "available" && state !== "downloading" && state !== "ready" && state !== "installing") return null;

  const version = rel.version;
  const manual = updates.install === "manual";
  let text: string;
  if (state === "error") text = t(updates.errorStage === "install" ? "desktop.updates.errors.install" : "desktop.updates.errors.download", { version });
  else if (manual) text = t("desktop.updates.banner.manual", { version });
  else if (state === "downloading") {
    const p = updates.progress;
    const percent = p && p.total > 0 ? formatNumber(p.written / p.total, { style: "percent" }) : "";
    text = t("desktop.updates.banner.downloading", { version, percent });
  } else text = t(`desktop.updates.banner.${state}`, { version });

  const progress = updates.progress;
  return (
    <Alert color={failed ? "red" : "blue"} py="xs" mb="sm" withCloseButton={false}>
      <Stack gap="xs">
        <Group justify="space-between" wrap="nowrap" gap="sm">
          <Text size="sm">{text}</Text>
          <Group gap="xs" wrap="nowrap" style={{ flexShrink: 0 }}>
            {!manual && state === "ready" && (
              <Button size="xs" loading={install.isPending} onClick={() => install.mutate()}>
                {t("desktop.updates.install")}
              </Button>
            )}
            {!manual && (state === "available" || failed) && (
              <Button size="xs" variant="light" loading={download.isPending} onClick={() => download.mutate()}>
                {failed ? t("desktop.updates.retry") : t("desktop.updates.download")}
              </Button>
            )}
            <Button size="xs" variant="subtle" loading={notes.isPending} onClick={() => notes.mutate()}>
              {t("desktop.updates.notes")}
            </Button>
            {state !== "installing" && (
              <Button size="xs" variant="subtle" color="gray" onClick={() => setDismissed(version)}>
                {t("desktop.updates.later")}
              </Button>
            )}
          </Group>
        </Group>
        {state === "downloading" && progress && progress.total > 0 && <Progress size="sm" value={(100 * progress.written) / progress.total} />}
        {error && (
          <Text size="xs" c="red">
            {error}
          </Text>
        )}
      </Stack>
    </Alert>
  );
}

// «Настройки» → «Обновления»: the mode, the version, the last check and
// «Проверить сейчас».
export function UpdateSettings({ updates, opened, onClose }: { updates: Updates | undefined; opened: boolean; onClose: () => void }) {
  const [error, setError] = useState<string | null>(null);
  const queryClient = useQueryClient();
  const setMode = useMutation({
    mutationFn: setUpdateMode,
    onSuccess: (u) => queryClient.setQueryData(updatesQueryKey, u),
    onError: (err) => setError(appErrorText(err)),
  });
  const check = useUpdateAction(checkUpdates, setError);

  return (
    <Modal opened={opened} onClose={onClose} title={t("desktop.settings")} centered size="lg">
      {updates && (
        <Stack gap="sm">
          <Title order={5}>{t("desktop.updates.title")}</Title>
          <Text size="sm">{t("desktop.updates.current", { version: updates.version })}</Text>
          {!updates.enabled ? (
            <Text size="sm" c="dimmed">
              {t("desktop.updates.dev")}
            </Text>
          ) : (
            <>
              <Radio.Group
                value={updates.mode}
                onChange={(v) => {
                  const next = UPDATE_MODES.find((m) => m === v);
                  if (next) setMode.mutate(next);
                }}
              >
                <Stack gap="xs">
                  {UPDATE_MODES.map((m) => (
                    <Radio key={m} value={m} label={t(`desktop.updates.modes.${m}`)} />
                  ))}
                </Stack>
              </Radio.Group>
              {updates.install === "manual" && (
                <Text size="sm" c="dimmed">
                  {t("desktop.updates.manual")}
                </Text>
              )}
              <Group justify="space-between" wrap="nowrap" gap="sm">
                <Stack gap={2}>
                  <Text size="sm" c="dimmed">
                    {updates.lastCheck ? t("desktop.updates.lastCheck", { time: formatDate(updates.lastCheck) }) : t("desktop.updates.neverChecked")}
                  </Text>
                  <UpdateResult updates={updates} />
                </Stack>
                <Button variant="default" loading={check.isPending || updates.state === "checking"} onClick={() => check.mutate()} style={{ flexShrink: 0 }}>
                  {t("desktop.updates.checkNow")}
                </Button>
              </Group>
            </>
          )}
          {error && (
            <Alert color="red" py="xs" withCloseButton onClose={() => setError(null)}>
              {error}
            </Alert>
          )}
        </Stack>
      )}
    </Modal>
  );
}

function UpdateResult({ updates }: { updates: Updates }) {
  const version = updates.release?.version ?? "";
  if (updates.state === "error") {
    const stage = updates.errorStage ?? "check";
    return (
      <Text size="sm" c="red">
        {t(`desktop.updates.errors.${stage}`, { version })}
      </Text>
    );
  }
  const text = t(`desktop.updates.result.${updates.state}`, { version });
  return text ? <Text size="sm">{text}</Text> : null;
}
