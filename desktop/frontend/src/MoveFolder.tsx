import { Alert, Button, Group, Modal, Stack, Text } from "@mantine/core";
import { useMutation } from "@tanstack/react-query";
import { useState } from "react";
import { t } from "@web/i18n";
import { FolderNote } from "./FolderNote";
import { appErrorText, checkMoveFolder, moveFolder, pickFolder, type Connection, type FolderCheck } from "./appApi";

// «Сменить папку»: a new or empty folder from the OS's dialog, checked by
// the app, then the move (internal/app Manager.MoveFolder), which stops
// sync for its duration.
export default function MoveFolder({
  conn,
  title,
  opened,
  onClose,
  onMoved,
}: {
  conn: Connection;
  title: string;
  opened: boolean;
  onClose: () => void;
  onMoved: () => void;
}) {
  const [target, setTarget] = useState<FolderCheck | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [picking, setPicking] = useState(false);
  const move = useMutation({
    mutationFn: (path: string) => moveFolder(conn.id, path),
    onSuccess: () => {
      onMoved();
      close();
    },
    onError: (err) => setError(appErrorText(err)),
  });
  const choose = async () => {
    setError(null);
    setPicking(true);
    try {
      const path = await pickFolder(conn.folder);
      if (path) setTarget(await checkMoveFolder(conn.id, path));
    } catch (err) {
      setError(appErrorText(err));
    } finally {
      setPicking(false);
    }
  };
  function close() {
    setTarget(null);
    setError(null);
    onClose();
  }
  const canMove = target !== null && target.code === undefined;
  return (
    <Modal opened={opened} onClose={move.isPending ? () => undefined : close} title={t("desktop.moveDialog.title", { name: title })} centered>
      <Stack gap="sm">
        <Text size="sm" style={{ wordBreak: "break-all" }}>
          {t("desktop.moveDialog.current", { path: conn.folder })}
        </Text>
        <Text size="sm" c="dimmed">
          {t("desktop.moveDialog.text")}
        </Text>
        <Group gap="xs" wrap="nowrap" align="flex-start">
          <Stack gap={4} flex={1} miw={0}>
            {target && (
              <>
                <Text size="sm" style={{ wordBreak: "break-all" }}>
                  {t("desktop.moveDialog.target", { path: target.path })}
                </Text>
                {target.code !== undefined && <FolderNote folder={target} />}
              </>
            )}
          </Stack>
          <Button variant="default" size="xs" loading={picking} disabled={move.isPending} onClick={() => void choose()} style={{ flexShrink: 0 }}>
            {t("desktop.folderPicker.choose")}
          </Button>
        </Group>
        {error && (
          <Alert color="red" py="xs">
            {error}
          </Alert>
        )}
        <Group justify="flex-end">
          <Button variant="default" onClick={close} disabled={move.isPending}>
            {t("desktop.cancel")}
          </Button>
          <Button loading={move.isPending} disabled={!canMove} onClick={() => target && move.mutate(target.path)}>
            {t("desktop.moveDialog.confirm")}
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}
