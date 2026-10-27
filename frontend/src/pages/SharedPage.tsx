import { Anchor, Badge, Group, Stack, Table, Text, Title } from "@mantine/core";
import { IconFile, IconFolderFilled, IconUsers } from "@tabler/icons-react";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router-dom";
import { EmptyState, ErrorState, PageLoader } from "../components/States";
import Viewer from "../components/Viewer";
import { editorMode, editorQueryKey, fetchEditor, openEditorTab } from "../editor";
import { contentUrl, download, fetchSharedWithMe, sharedWithMeQueryKey, type SharedItem } from "../files";
import { formatDate, t } from "../i18n";
import { useIsPhone } from "../media";
import { inlineUrl, previewUrl, viewerKind } from "../preview";
import { folderPath } from "./FilesPage";

function levelText(item: SharedItem): string {
  return item.permissions.includes("write") ? t("share.edit") : t("share.view");
}

// "Shared with me" (R9): what others shared with the user or their groups.
// A folder opens in the file manager, starting from the shared folder; a
// file opens as it does there, so a shared document can be edited together.
export default function SharedPage() {
  const shared = useQuery({ queryKey: sharedWithMeQueryKey, queryFn: ({ signal }) => fetchSharedWithMe(signal) });
  const editor = useQuery({ queryKey: editorQueryKey, queryFn: ({ signal }) => fetchEditor(signal), staleTime: 10 * 60 * 1000, retry: false });
  const phone = useIsPhone();
  const viewables = (shared.data ?? []).filter((n) => viewerKind(n));
  const [viewing, setViewing] = useState<number | null>(null);
  const open = (n: SharedItem) => {
    if (editorMode(editor.data, n.name)) openEditorTab(n.id);
    else if (viewerKind(n)) setViewing(viewables.findIndex((v) => v.id === n.id));
    else download(contentUrl(n.id));
  };

  if (shared.isError && shared.data === undefined) {
    return <ErrorState error={shared.error} onRetry={() => void shared.refetch()} />;
  }
  return (
    <Stack gap="sm">
      <Title order={3}>{t("shared.title")}</Title>
      {shared.isPending ? (
        <PageLoader />
      ) : shared.data.length === 0 ? (
        <EmptyState icon={<IconUsers size={36} stroke={1.5} />} color="gray" title={t("shared.emptyTitle")} description={t("shared.emptyDescription")} />
      ) : (
        <Table highlightOnHover verticalSpacing="xs">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>{t("files.columns.name")}</Table.Th>
              {/* A phone shows the owner and the access under the name. */}
              {!phone && <Table.Th>{t("shared.owner")}</Table.Th>}
              {!phone && <Table.Th visibleFrom="xs">{t("shared.access")}</Table.Th>}
              {!phone && (
                <Table.Th visibleFrom="sm" ta="right">
                  {t("shared.sharedAt")}
                </Table.Th>
              )}
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {shared.data.map((item) => (
              <Table.Tr key={item.id}>
                {/* No width of its own on a phone: the name truncates
                    instead of widening the table past the screen. */}
                <Table.Td style={phone ? { maxWidth: 0, width: "100%" } : undefined}>
                  <Group gap="xs" wrap="nowrap">
                    {item.kind === "dir" ? (
                      <IconFolderFilled size={18} color="var(--mantine-color-yellow-6)" aria-hidden />
                    ) : (
                      <IconFile size={18} aria-hidden />
                    )}
                    {item.kind === "dir" ? (
                      <Anchor component={Link} to={folderPath(item.id, "")} size="sm" truncate={phone ? "end" : undefined}>
                        {item.name}
                      </Anchor>
                    ) : (
                      <Anchor component="button" type="button" size="sm" truncate={phone ? "end" : undefined} onClick={() => open(item)}>
                        {item.name}
                      </Anchor>
                    )}
                  </Group>
                  {phone && (
                    <Group gap="xs" wrap="nowrap" mt={4}>
                      <Text size="xs" c="dimmed" truncate="end">
                        {item.owner.name}
                      </Text>
                      <Badge variant="light" size="sm" color={item.permissions.includes("write") ? "blue" : "gray"} style={{ flexShrink: 0 }}>
                        {levelText(item)}
                      </Badge>
                    </Group>
                  )}
                </Table.Td>
                {!phone && (
                  <>
                    <Table.Td>
                      <Text size="sm">{item.owner.name}</Text>
                    </Table.Td>
                    <Table.Td visibleFrom="xs">
                      <Badge variant="light" color={item.permissions.includes("write") ? "blue" : "gray"}>
                        {levelText(item)}
                      </Badge>
                    </Table.Td>
                    <Table.Td visibleFrom="sm" ta="right">
                      <Text size="sm" c="dimmed">
                        {formatDate(item.sharedAt)}
                      </Text>
                    </Table.Td>
                  </>
                )}
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      )}
      {viewing !== null && (
        <Viewer
          items={viewables}
          index={viewing}
          onIndex={setViewing}
          onClose={() => setViewing(null)}
          imageUrl={(n) => previewUrl(n.id, "l", n.etag)}
          pdfUrl={(n) => inlineUrl(n.id)}
          textUrl={(n) => contentUrl(n.id)}
          onDownload={(n) => download(contentUrl(n.id))}
        />
      )}
    </Stack>
  );
}
