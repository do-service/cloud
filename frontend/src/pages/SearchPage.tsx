import { Anchor, Button, Center, Group, Stack, Table, Text, TextInput, Title } from "@mantine/core";
import { IconSearch, IconSearchOff } from "@tabler/icons-react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { NodeIcon, Thumb } from "../components/FileList";
import { EmptyState, ErrorState, PageLoader } from "../components/States";
import Viewer from "../components/Viewer";
import { editorMode, editorQueryKey, fetchEditor, openEditorTab } from "../editor";
import { contentUrl, download, fetchSearch, MIN_SEARCH_QUERY, ROOT, searchQueryKey, type SearchResult } from "../files";
import { formatBytes, formatDate, t } from "../i18n";
import { useIsPhone } from "../media";
import { inlineUrl, previewable, previewUrl, viewerKind } from "../preview";
import { folderPath } from "./FilesPage";

// The folder a result is in, as a link there.
function folderLink(n: SearchResult, size: "xs" | "sm") {
  return (
    <Anchor
      component={Link}
      // The root's own id would work too; /files is its usual address.
      to={folderPath(n.path === "" ? ROOT : n.parentId, "")}
      size={size}
      c="dimmed"
      truncate="end"
      aria-label={t("search.openFolder", { name: n.path || t("files.root") })}
    >
      {n.path || t("files.root")}
    </Anchor>
  );
}

// Search by name (R5, E3-7): ?q= is the source of truth, so a reload, Back
// or a shared address shows the same results. Only the user's own tree is
// searched, which the note under the results says.
export default function SearchPage() {
  const [params, setParams] = useSearchParams();
  const navigate = useNavigate();
  const phone = useIsPhone();
  const q = params.get("q") ?? "";
  const query = q.trim();
  // Counted in code points, as the backend counts runes.
  const valid = Array.from(query).length >= MIN_SEARCH_QUERY;

  // The page's own field, for narrow screens where the header has none.
  const [value, setValue] = useState(q);
  const [shown, setShown] = useState(q);
  if (q !== shown) {
    setShown(q);
    setValue(q);
  }

  const editor = useQuery({ queryKey: editorQueryKey, queryFn: ({ signal }) => fetchEditor(signal), staleTime: 10 * 60 * 1000, retry: false });
  const results = useInfiniteQuery({
    queryKey: searchQueryKey(query),
    queryFn: ({ pageParam, signal }) => fetchSearch(query, pageParam || null, signal),
    initialPageParam: "",
    getNextPageParam: (last) => last.nextCursor ?? undefined,
    // A short query is refused by the backend anyway: the hint says why.
    enabled: valid,
  });
  const items = results.data?.pages.flatMap((p) => p.items) ?? [];

  // Opens a result the way the file manager does.
  const viewables = items.filter((n) => viewerKind(n));
  const [viewing, setViewing] = useState<number | null>(null);
  const open = (n: SearchResult) => {
    if (n.kind === "dir") navigate(folderPath(n.id, ""));
    else if (editorMode(editor.data, n.name)) openEditorTab(n.id);
    else if (viewerKind(n)) setViewing(viewables.findIndex((v) => v.id === n.id));
    else download(contentUrl(n.id));
  };

  const field = (
    <TextInput
      hiddenFrom="sm"
      type="search"
      aria-label={t("search.label")}
      placeholder={t("search.placeholder")}
      leftSection={<IconSearch size={16} aria-hidden />}
      // Coming here from the header's icon means wanting to type.
      autoFocus={q === ""}
      value={value}
      onChange={(e) => setValue(e.currentTarget.value)}
      onKeyDown={(e) => {
        if (e.key === "Enter") setParams({ q: value });
      }}
    />
  );
  const note = (
    <Text size="sm" c="dimmed" ta="center">
      {t("search.ownOnly")}
    </Text>
  );

  let body: ReactNode;
  if (!valid) {
    body = <EmptyState icon={<IconSearch size={36} stroke={1.5} />} color="gray" title={t("search.hintTitle")} description={t("search.hintDescription")} />;
  } else if (results.isError && results.data === undefined) {
    // A failed next page keeps the loaded results; only a search that never
    // loaded is replaced by the error.
    body = <ErrorState error={results.error} onRetry={() => void results.refetch()} />;
  } else if (results.isPending) {
    body = <PageLoader />;
  } else if (items.length === 0) {
    body = (
      <EmptyState
        icon={<IconSearchOff size={36} stroke={1.5} />}
        color="gray"
        title={t("search.noResults", { q: query })}
        description={t("search.noResultsDescription")}
      />
    );
  } else {
    body = (
      <Table highlightOnHover verticalSpacing="xs">
        <Table.Thead>
          <Table.Tr>
            <Table.Th>{t("files.columns.name")}</Table.Th>
            {/* A phone shows the folder, size and date under the name. */}
            {!phone && <Table.Th visibleFrom="sm">{t("search.folder")}</Table.Th>}
            {!phone && (
              <Table.Th visibleFrom="xs" ta="right">
                {t("files.columns.size")}
              </Table.Th>
            )}
            {!phone && <Table.Th ta="right">{t("files.columns.mtime")}</Table.Th>}
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {items.map((n) => (
            <Table.Tr key={n.id}>
              {/* No width of its own on a phone: the name truncates instead
                  of widening the table past the screen. */}
              <Table.Td style={phone ? { maxWidth: 0, width: "100%" } : undefined}>
                <Group gap="xs" wrap="nowrap">
                  {previewable(n) ? <Thumb key={n.etag} node={n} src={previewUrl(n.id, "s", n.etag)} /> : <NodeIcon node={n} />}
                  {n.kind === "dir" ? (
                    <Anchor component={Link} to={folderPath(n.id, "")} size="sm" truncate="end" title={n.name}>
                      {n.name}
                    </Anchor>
                  ) : (
                    <Anchor component="button" type="button" size="sm" truncate="end" title={n.name} onClick={() => open(n)}>
                      {n.name}
                    </Anchor>
                  )}
                </Group>
                {phone && (
                  <Group gap={4} wrap="nowrap" mt={2}>
                    {folderLink(n, "xs")}
                    <Text size="xs" c="dimmed" style={{ flexShrink: 0 }}>
                      · {formatBytes(n.size)} · {formatDate(n.mtime)}
                    </Text>
                  </Group>
                )}
              </Table.Td>
              {!phone && (
                <>
                  <Table.Td visibleFrom="sm">{folderLink(n, "sm")}</Table.Td>
                  <Table.Td visibleFrom="xs" ta="right">
                    <Text size="sm" c="dimmed">
                      {formatBytes(n.size)}
                    </Text>
                  </Table.Td>
                  <Table.Td ta="right">
                    <Text size="sm" c="dimmed">
                      {formatDate(n.mtime)}
                    </Text>
                  </Table.Td>
                </>
              )}
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    );
  }

  // Its retry is the same "more" request, so it takes the button's place.
  const moreFailed = results.isFetchNextPageError && !results.isFetchingNextPage;
  return (
    <Stack gap="sm">
      <Title order={3}>{t("search.title")}</Title>
      {field}
      {body}
      {moreFailed && <ErrorState compact error={results.error} onRetry={() => void results.fetchNextPage()} />}
      {results.hasNextPage && !moreFailed && (
        <Center>
          <Button variant="subtle" loading={results.isFetchingNextPage} onClick={() => void results.fetchNextPage()}>
            {t("search.more")}
          </Button>
        </Center>
      )}
      {note}
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
