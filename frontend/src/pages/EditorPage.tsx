import { Box, Button, Center } from "@mantine/core";
import { IconX } from "@tabler/icons-react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useRef } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { ErrorState, PageLoader } from "../components/States";
import { openEditor } from "../editor";
import { fetchNode, nodeQueryKey, ROOT } from "../files";
import { t } from "../i18n";
import { folderPath } from "./FilesPage";

const FRAME = "editor-frame";

// A document open in the online editor (E9-9). The editor runs in a frame
// on its own origin: the access token is posted to it in a form, never in
// a URL, so it stays out of the editor's logs and the browser history.
// It fills its own tab (see openEditorTab), the editor's bar its only one.
export default function EditorPage() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const form = useRef<HTMLFormElement>(null);
  const node = useQuery({ queryKey: nodeQueryKey(id), queryFn: ({ signal }) => fetchNode(id, signal) });
  // A fresh session each time the page opens: tokens aren't cached.
  const session = useQuery({ queryKey: ["edit-session", id], queryFn: () => openEditor(id), gcTime: 0, staleTime: Infinity, retry: false });

  const path = node.data?.path ?? [];
  const parent = path.length > 1 ? path[path.length - 2] : undefined;
  const back = folderPath(parent && parent.name !== "" ? parent.id : ROOT, "");
  const name = node.data?.name;

  // The tab is the document: it's named after it.
  useEffect(() => {
    if (!name) return;
    const before = document.title;
    document.title = name;
    return () => {
      document.title = before;
    };
  }, [name]);

  // The app opens the editor in a tab of its own, which script may close.
  // A tab the user opened (a pasted address, a document made in this tab)
  // the browser keeps open, close() doing nothing: then the folder is the
  // way out, as without tabs.
  const close = useCallback(() => {
    window.close();
    if (!window.closed) navigate(back);
  }, [back, navigate]);

  useEffect(() => {
    if (session.data) form.current?.submit();
  }, [session.data]);

  // The editor's close button, and its saves, come as messages.
  useEffect(() => {
    if (!session.data) return;
    const origin = new URL(session.data.url).origin;
    const onMessage = (e: MessageEvent) => {
      if (e.origin !== origin || typeof e.data !== "string") return;
      let msg: unknown;
      try {
        msg = JSON.parse(e.data);
      } catch {
        return;
      }
      if (typeof msg !== "object" || msg === null || !("MessageId" in msg)) return;
      if (msg.MessageId === "UI_Close") close();
      // A save makes a new version: what the file manager shows changed.
      if (msg.MessageId === "Doc_ModifiedStatus" || msg.MessageId === "Action_Save_Resp") {
        void queryClient.invalidateQueries({ queryKey: ["children"] });
      }
    };
    window.addEventListener("message", onMessage);
    return () => window.removeEventListener("message", onMessage);
  }, [session.data, close, queryClient]);

  const error = session.error ?? (node.data === undefined ? node.error : null);
  // A new session is a new token: retrying asks for one again.
  const retry = () => {
    if (session.isError) void session.refetch();
    if (node.isError) void node.refetch();
  };
  if (error) {
    return (
      <Center h="100dvh">
        <ErrorState
          error={error}
          onRetry={retry}
          action={
            <Button variant="default" leftSection={<IconX size={16} />} onClick={close}>
              {t("editor.close")}
            </Button>
          }
        />
      </Center>
    );
  }
  if (!session.data) {
    return (
      <Center h="100dvh">
        <PageLoader />
      </Center>
    );
  }
  return (
    <Box h="100dvh" style={{ overflow: "hidden" }}>
      <form ref={form} method="post" action={`${session.data.url}&closebutton=1`} target={FRAME} hidden>
        <input type="hidden" name="access_token" value={session.data.accessToken} />
        <input type="hidden" name="access_token_ttl" value={String(session.data.accessTokenTtl)} />
      </form>
      <Box
        component="iframe"
        name={FRAME}
        title={t("editor.frameTitle", { name: name ?? "" })}
        allow="clipboard-read; clipboard-write; fullscreen"
        style={{ display: "block", width: "100%", height: "100%", border: 0 }}
      />
    </Box>
  );
}
