import { Alert, Anchor, Button, Group, Input, Loader, Modal, Stack, Text, TextInput } from "@mantine/core";
import { useDebouncedValue } from "@mantine/hooks";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type FormEvent } from "react";
import { ApiError } from "@web/api";
import { t } from "@web/i18n";
import { FolderNote } from "./FolderNote";
import { appErrorText, cancelSignIn, fetchSignIn, newFolder, pickFolder, signInErrorText, startSignIn } from "./appApi";

// «Добавить облако»: the server's address, then the sign-in in the
// system browser on the cloud's own page; the app waits for it to come
// back (internal/signin), with «Отмена».
export default function AddCloud({ opened, onClose, onAdded }: { opened: boolean; onClose: () => void; onAdded: (connId: string) => void }) {
  const [server, setServer] = useState("");
  const [waiting, setWaiting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [starting, setStarting] = useState(false);
  // The folder the user picked; "" is the default for the address.
  const [folder, setFolder] = useState("");
  const [debouncedServer] = useDebouncedValue(server.trim(), 400);
  const queryClient = useQueryClient();
  const check = useQuery({
    queryKey: ["app", "new-folder", debouncedServer, folder],
    queryFn: () => newFolder(debouncedServer, folder),
    enabled: opened && debouncedServer !== "",
    retry: false,
    gcTime: 0,
  });
  const choose = async () => {
    setError(null);
    try {
      const path = await pickFolder(check.data?.path ?? "");
      if (path) setFolder(path);
    } catch (err) {
      setError(appErrorText(err));
    }
  };

  const signin = useQuery({
    queryKey: ["app", "signin"],
    queryFn: fetchSignIn,
    enabled: waiting,
    refetchInterval: 1000,
    gcTime: 0,
  });
  // The outcome, once: the app forgets it, so the next dialog starts
  // clean.
  const outcome = waiting ? signin.data : undefined;
  useEffect(() => {
    if (outcome?.state === "done" && outcome.connId) {
      setWaiting(false);
      void cancelSignIn();
      onAdded(outcome.connId);
    } else if (outcome?.state === "error") {
      setWaiting(false);
      setError(signInErrorText(outcome.code));
      void cancelSignIn();
    }
  }, [outcome, onAdded]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    setStarting(true);
    try {
      await startSignIn(server, folder);
      // A previous attempt's outcome mustn't end this one.
      queryClient.removeQueries({ queryKey: ["app", "signin"] });
      setWaiting(true);
    } catch (err) {
      setError(err instanceof ApiError ? signInErrorText(err.code) : signInErrorText(undefined));
    } finally {
      setStarting(false);
    }
  };
  const close = () => {
    if (waiting) void cancelSignIn();
    setWaiting(false);
    setError(null);
    setFolder("");
    onClose();
  };

  return (
    <Modal opened={opened} onClose={close} title={t("desktop.signin.title")} centered>
      {waiting ? (
        <Stack gap="sm">
          <Group gap="sm" wrap="nowrap">
            <Loader size="sm" />
            <Text size="sm">{t("desktop.signin.waiting")}</Text>
          </Group>
          {signin.data?.url && (
            <Text size="xs" c="dimmed" style={{ wordBreak: "break-all" }}>
              {t("desktop.signin.notOpened")}{" "}
              <Anchor href={signin.data.url} target="_blank" size="xs">
                {signin.data.url}
              </Anchor>
            </Text>
          )}
          <Group justify="flex-end">
            <Button variant="default" onClick={close}>
              {t("desktop.cancel")}
            </Button>
          </Group>
        </Stack>
      ) : (
        <form onSubmit={(e) => void submit(e)}>
          <Stack gap="sm">
            <TextInput
              label={t("desktop.signin.server")}
              placeholder={t("desktop.signin.placeholder")}
              description={t("desktop.signin.hint")}
              value={server}
              onChange={(e) => setServer(e.currentTarget.value)}
              data-autofocus
              required
            />
            {check.data && (
              <Input.Wrapper label={t("desktop.folderPicker.label")}>
                <Stack gap={4}>
                  <Group gap="xs" wrap="nowrap">
                    {/* Cut from the start, where a long path is least telling; the
                        mark keeps the leading "/" in place in the RTL box that
                        truncate="start" makes. */}
                    <Text size="sm" flex={1} miw={0} truncate="start" title={check.data.path}>
                      {`\u200E${check.data.path}`}
                    </Text>
                    <Button variant="default" size="xs" onClick={() => void choose()} style={{ flexShrink: 0 }}>
                      {t("desktop.folderPicker.choose")}
                    </Button>
                  </Group>
                  <FolderNote folder={check.data} />
                </Stack>
              </Input.Wrapper>
            )}
            {error && (
              <Alert color="red" py="xs">
                {error}
              </Alert>
            )}
            <Group justify="flex-end">
              <Button variant="default" onClick={close}>
                {t("desktop.cancel")}
              </Button>
              <Button type="submit" loading={starting} disabled={check.data?.code !== undefined}>
                {t("desktop.signin.submit")}
              </Button>
            </Group>
          </Stack>
        </form>
      )}
    </Modal>
  );
}
