import { ActionIcon, Box, Button, Center, Group, Loader, Modal, Text, Tooltip } from "@mantine/core";
import { IconChevronLeft, IconChevronRight, IconDownload, IconX } from "@tabler/icons-react";
import { useQuery } from "@tanstack/react-query";
import { useEffect, useRef, useState, type PointerEvent } from "react";
import type { FileNode } from "../files";
import { t } from "../i18n";
import { useIsPhone } from "../media";
import { fetchTextHead, TEXT_LIMIT, textQueryKey, viewerKind } from "../preview";
import { ErrorState, PageLoader } from "./States";
import classes from "./Viewer.module.css";

type Props = {
  // The files the viewer steps through (images, PDFs, text), in list order.
  items: FileNode[];
  index: number;
  onIndex: (index: number) => void;
  onClose: () => void;
  // Where to show each: an image's large preview, a PDF's content.
  imageUrl: (n: FileNode) => string;
  pdfUrl: (n: FileNode) => string;
  // A text file's content, read by the viewer; without it text isn't shown.
  textUrl?: (n: FileNode) => string;
  // Downloading, when allowed.
  onDownload?: (n: FileNode) => void;
};

// A swipe shorter than this (px) is a tap that slipped.
const SWIPE_MIN = 50;

// Viewer shows images, PDFs and text over the page, one at a time: arrows (on
// screen and on the keyboard) step through the folder's others, a
// gallery of what's in it; a finger swipes between them too.
export default function Viewer({ items, index, onIndex, onClose, imageUrl, pdfUrl, textUrl, onDownload }: Props) {
  const phone = useIsPhone();
  const swipe = useRef<{ x: number; y: number } | null>(null);
  const n = items[index];
  const prev = index > 0 ? index - 1 : undefined;
  const next = index < items.length - 1 ? index + 1 : undefined;

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "ArrowLeft" && prev !== undefined) onIndex(prev);
      if (e.key === "ArrowRight" && next !== undefined) onIndex(next);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [prev, next, onIndex]);

  if (!n) return null;
  const kind = viewerKind(n);
  const swipeStart = (e: PointerEvent) => {
    swipe.current = e.pointerType === "touch" ? { x: e.clientX, y: e.clientY } : null;
  };
  // Mostly sideways and far enough; a vertical one scrolls text instead.
  const swipeEnd = (e: PointerEvent) => {
    const from = swipe.current;
    swipe.current = null;
    if (!from) return;
    const dx = e.clientX - from.x;
    if (Math.abs(dx) < SWIPE_MIN || Math.abs(dx) < 2 * Math.abs(e.clientY - from.y)) return;
    if (dx > 0 && prev !== undefined) onIndex(prev);
    if (dx < 0 && next !== undefined) onIndex(next);
  };
  return (
    <Modal opened onClose={onClose} fullScreen withCloseButton={false} padding={0} classNames={{ body: classes.body, content: classes.content }} title={n.name}>
      <Group className={classes.bar} justify="space-between" wrap="nowrap" px="md" py="xs">
        <Group gap="sm" wrap="nowrap" style={{ minWidth: 0 }}>
          <Text fw={600} truncate="end">
            {n.name}
          </Text>
          {items.length > 1 && (
            <Text size="sm" c="dimmed" style={{ flex: "none" }}>
              {t("viewer.counter", { index: index + 1, total: items.length })}
            </Text>
          )}
        </Group>
        <Group gap="xs" wrap="nowrap">
          {onDownload &&
            (phone ? (
              <ActionIcon variant="default" size="lg" onClick={() => onDownload(n)} aria-label={t("actions.download")}>
                <IconDownload size={18} />
              </ActionIcon>
            ) : (
              <Button variant="default" size="xs" leftSection={<IconDownload size={16} />} onClick={() => onDownload(n)}>
                {t("actions.download")}
              </Button>
            ))}
          <Tooltip label={t("viewer.close")}>
            <ActionIcon variant="subtle" color="gray" size="lg" onClick={onClose} aria-label={t("viewer.close")}>
              <IconX size={20} />
            </ActionIcon>
          </Tooltip>
        </Group>
      </Group>
      <Box
        className={classes.stage}
        onPointerDown={swipeStart}
        onPointerUp={swipeEnd}
        onPointerCancel={() => {
          swipe.current = null;
        }}
      >
        {kind === "text" ? (
          textUrl ? (
            <TextView key={`${n.id}-${n.etag}`} node={n} url={textUrl(n)} onDownload={onDownload && (() => onDownload(n))} />
          ) : (
            <Center className={classes.overlay}>
              <Text c="dimmed">{t("viewer.failed")}</Text>
            </Center>
          )
        ) : kind === "pdf" ? (
          <iframe key={n.id} src={pdfUrl(n)} title={n.name} className={classes.pdf} />
        ) : (
          <Picture key={`${n.id}-${n.etag}`} src={imageUrl(n)} alt={n.name} />
        )}
        {prev !== undefined && (
          <ActionIcon className={classes.prev} variant="filled" color="dark" size="xl" radius="xl" onClick={() => onIndex(prev)} aria-label={t("viewer.prev")}>
            <IconChevronLeft />
          </ActionIcon>
        )}
        {next !== undefined && (
          <ActionIcon className={classes.next} variant="filled" color="dark" size="xl" radius="xl" onClick={() => onIndex(next)} aria-label={t("viewer.next")}>
            <IconChevronRight />
          </ActionIcon>
        )}
      </Box>
    </Modal>
  );
}

function Picture({ src, alt }: { src: string; alt: string }) {
  const [state, setState] = useState<"loading" | "ok" | "failed">("loading");
  return (
    <>
      {state === "loading" && (
        <Center className={classes.overlay}>
          <Loader aria-label={t("common.loading")} />
        </Center>
      )}
      {state === "failed" ? (
        <Center className={classes.overlay}>
          <Text c="dimmed">{t("viewer.failed")}</Text>
        </Center>
      ) : (
        <img src={src} alt={alt} className={classes.image} data-loaded={state === "ok" || undefined} onLoad={() => setState("ok")} onError={() => setState("failed")} />
      )}
    </>
  );
}

// TextView shows the start of a text file as plain text: React puts it in
// as text, so markup in it (an .html file's) is shown, never run.
function TextView({ node, url, onDownload }: { node: FileNode; url: string; onDownload?: () => void }) {
  const text = useQuery({
    queryKey: textQueryKey(url, node.etag, TEXT_LIMIT),
    queryFn: ({ signal }) => fetchTextHead(url, TEXT_LIMIT, signal),
    // An empty file has no range to ask for: the server would answer 416.
    enabled: node.size > 0,
  });
  if (node.size === 0) {
    return (
      <Center className={classes.overlay}>
        <Text c="dimmed">{t("viewer.empty")}</Text>
      </Center>
    );
  }
  if (text.isPending) {
    return (
      <Center className={classes.overlay}>
        <PageLoader />
      </Center>
    );
  }
  if (text.isError) {
    return (
      <Center className={classes.overlay}>
        <ErrorState compact error={text.error} onRetry={() => void text.refetch()} />
      </Center>
    );
  }
  return (
    <Box className={classes.textBox}>
      {text.data.truncated && (
        <Group className={classes.notice} justify="center" gap="sm">
          <Text size="sm">{t("viewer.truncated")}</Text>
          {onDownload && (
            <Button variant="default" size="compact-sm" leftSection={<IconDownload size={14} />} onClick={onDownload}>
              {t("actions.download")}
            </Button>
          )}
        </Group>
      )}
      <pre className={classes.text}>{text.data.text}</pre>
    </Box>
  );
}
