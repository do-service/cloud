import { Box, Button, Center, Group, Loader, Stack, Text, UnstyledButton } from "@mantine/core";
import { IconEye, IconFolderOpen } from "@tabler/icons-react";
import { useQuery } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";
import { contentUrl, fetchVersions, versionsQueryKey, type FileNode } from "../files";
import { formatBytes, formatDate, t } from "../i18n";
import { typeLabel } from "../fileTypes";
import { fetchTextHead, previewable, previewUrl, TEXT_PEEK, textQueryKey, viewerKind } from "../preview";
import { NodeIcon } from "./FileList";
import classes from "./DetailsPanel.module.css";

type Props = {
  selection: FileNode[];
  // What a double click on the row does.
  onOpen: (node: FileNode) => void;
  // The toolbar's buttons for a single file (download, versions), so the
  // two never disagree on what's allowed.
  actions?: ReactNode;
};

// The side panel of the file manager: what's selected, larger and with its
// details — one file's preview, or what a multiple selection adds up to.
export default function DetailsPanel({ selection, onOpen, actions }: Props) {
  let body: ReactNode;
  if (selection.length === 0) {
    body = (
      <Center h="100%" p="md">
        <Text c="dimmed" ta="center" size="sm">
          {t("files.panel.hint")}
        </Text>
      </Center>
    );
  } else if (selection.length > 1) {
    const total = selection.reduce((sum, n) => sum + n.size, 0);
    body = (
      <Stack gap={4} p="md">
        <Text fw={600}>{t("files.selected", { count: selection.length })}</Text>
        <Text size="sm" c="dimmed">
          {t("files.panel.total", { size: formatBytes(total) })}
        </Text>
      </Stack>
    );
  } else {
    body = <Single key={selection[0].id} node={selection[0]} onOpen={onOpen} actions={actions} />;
  }
  return (
    <Box component="aside" className={classes.panel} aria-label={t("files.panel.label")}>
      {body}
    </Box>
  );
}

function Single({ node: n, onOpen, actions }: { node: FileNode; onOpen: (node: FileNode) => void; actions?: ReactNode }) {
  const file = n.kind === "file";
  // The dialog's query: opening "Версии" afterwards finds it loaded.
  const versions = useQuery({
    queryKey: versionsQueryKey(n.id),
    queryFn: ({ signal }) => fetchVersions(n.id, signal),
    enabled: file,
    retry: false,
  });
  const author = versions.data?.currentAuthor;

  let preview: ReactNode;
  if (previewable(n)) preview = <Picture key={n.etag} node={n} />;
  else if (viewerKind(n) === "text") preview = <TextPeek node={n} />;
  else preview = <NodeIcon node={n} size={96} />;

  return (
    <Stack gap="sm" p="sm">
      <UnstyledButton className={classes.preview} onClick={() => onOpen(n)} aria-label={t("files.panel.openPreview", { name: n.name })}>
        {preview}
      </UnstyledButton>
      <Text fw={600} className={classes.name}>
        {n.name}
      </Text>
      <Stack gap={6}>
        {/* The raw MIME stays at hand, for those who look for it. */}
        <Detail label={t("files.panel.type")} value={typeLabel(n)} title={n.mime?.split(";")[0]} />
        <Detail label={t("files.panel.size")} value={formatBytes(n.size)} />
        <Detail label={t("files.panel.mtime")} value={formatDate(n.mtime)} />
        {author && <Detail label={t("files.panel.author")} value={author.displayName} />}
        {n.shared && <Detail label={t("files.panel.sharing")} value={t("files.panel.shared")} />}
      </Stack>
      <Group gap="xs">
        <Button size="xs" leftSection={file ? <IconEye size={16} /> : <IconFolderOpen size={16} />} onClick={() => onOpen(n)}>
          {t("files.panel.open")}
        </Button>
        {file && actions}
      </Group>
    </Stack>
  );
}

function Detail({ label, value, title }: { label: string; value: string; title?: string }) {
  return (
    <Group gap="xs" justify="space-between" wrap="nowrap" align="flex-start">
      <Text size="sm" c="dimmed" style={{ flex: "none" }}>
        {label}
      </Text>
      <Text size="sm" ta="right" className={classes.value} title={title}>
        {value}
      </Text>
    </Group>
  );
}

// The large preview, the icon until it loads and if there is none.
function Picture({ node }: { node: FileNode }) {
  const [state, setState] = useState<"loading" | "ok" | "failed">("loading");
  if (state === "failed") return <NodeIcon node={node} size={96} />;
  return (
    <>
      {state === "loading" && <NodeIcon node={node} size={96} />}
      <img
        src={previewUrl(node.id, "l", node.etag)}
        alt=""
        draggable={false}
        className={classes.image}
        data-loaded={state === "ok" || undefined}
        onLoad={() => setState("ok")}
        onError={() => setState("failed")}
      />
    </>
  );
}

// The first few KB of a text file: enough to recognize it.
function TextPeek({ node }: { node: FileNode }) {
  const url = contentUrl(node.id);
  const text = useQuery({
    queryKey: textQueryKey(url, node.etag, TEXT_PEEK),
    queryFn: ({ signal }) => fetchTextHead(url, TEXT_PEEK, signal),
    enabled: node.size > 0,
    retry: false,
  });
  if (node.size === 0) {
    return (
      <Text size="sm" c="dimmed">
        {t("viewer.empty")}
      </Text>
    );
  }
  if (text.isPending) return <Loader size="sm" aria-label={t("common.loading")} />;
  if (text.isError) return <NodeIcon node={node} size={96} />;
  return <pre className={classes.text}>{text.data.text}</pre>;
}
