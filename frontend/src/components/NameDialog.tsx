import { Button, Group, Modal, Stack, TextInput } from "@mantine/core";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { errorText } from "../api";
import { t } from "../i18n";
import { useIsPhone } from "../media";

type Props = {
  opened: boolean;
  title: string;
  submitLabel: string;
  // Prefilled name; renaming preselects it up to the extension, as file
  // managers do, so typing replaces the name and keeps ".pdf".
  initial?: string;
  onClose: () => void;
  // Rejects with the API error to show under the field (a name conflict,
  // say); the dialog stays open for another try.
  onSubmit: (name: string) => Promise<void>;
};

export default function NameDialog({ opened, title, submitLabel, initial = "", onClose, onSubmit }: Props) {
  const phone = useIsPhone();
  const [name, setName] = useState(initial);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // The preselection happens once per opening, on the focus Modal gives
  // the field (data-autofocus) once its enter transition has mounted it.
  const preselected = useRef(false);

  useEffect(() => {
    if (!opened) return;
    setName(initial);
    setError(null);
    preselected.current = false;
  }, [opened, initial]);

  const preselect = (field: HTMLInputElement) => {
    if (preselected.current) return;
    preselected.current = true;
    const dot = field.value.lastIndexOf(".");
    field.setSelectionRange(0, dot > 0 ? dot : field.value.length);
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      await onSubmit(name.trim());
      onClose();
    } catch (err) {
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal opened={opened} onClose={onClose} title={title} fullScreen={phone}>
      <form onSubmit={submit}>
        <Stack>
          <TextInput
            onFocus={(e) => preselect(e.currentTarget)}
            label={t("actions.name")}
            value={name}
            onChange={(e) => {
              setName(e.currentTarget.value);
              setError(null);
            }}
            error={error}
            data-autofocus
            required
          />
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              {t("actions.cancel")}
            </Button>
            <Button type="submit" loading={busy} disabled={name.trim() === "" || name.trim() === initial}>
              {submitLabel}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}
