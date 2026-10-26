import { Text } from "@mantine/core";
import { formatNumber, t } from "@web/i18n";
import { folderErrorText, type FolderCheck } from "./appApi";

// What a checked folder means for the user: why it can't be used, or
// what happens with what is in it.
export function FolderNote({ folder }: { folder: FolderCheck }) {
  if (folder.code !== undefined) {
    return (
      <Text size="xs" c="red">
        {folderErrorText(folder.code) ?? t("errors.unknown")}
      </Text>
    );
  }
  let note: string;
  if (!folder.exists) note = t("desktop.folderPicker.willCreate");
  else if (folder.more) note = t("desktop.folderPicker.notEmptyMore", { count: formatNumber(folder.files + folder.folders) });
  else if (folder.files + folder.folders > 0)
    note = t("desktop.folderPicker.notEmpty", { files: formatNumber(folder.files), folders: formatNumber(folder.folders) });
  else note = t("desktop.folderPicker.empty");
  return (
    <>
      <Text size="xs" c="dimmed">
        {note}
      </Text>
      {folder.replace && (
        <Text size="xs" c="dimmed">
          {t("desktop.folderPicker.replace")}
        </Text>
      )}
    </>
  );
}
