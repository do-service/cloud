import { apiRequest } from "./api";
import { expectShape, isNumber, isRecord, isString } from "./guards";

// The online editor (R12, E9-9): which files it opens, and the way in.

export type EditorMode = "edit" | "view";

// Extension (lower case, no dot) → what the editor does with it; empty
// when there is no editor.
export type EditorInfo = Record<string, EditorMode>;

// Mirrors backend/internal/wopi editResponse.
export type EditSession = { url: string; accessToken: string; accessTokenTtl: number };

export const editorQueryKey = ["editor"] as const;

function isEditorMode(v: unknown): v is EditorMode {
  return v === "edit" || v === "view";
}

function isEditorInfo(v: unknown): v is { extensions: EditorInfo } {
  return isRecord(v) && isRecord(v.extensions) && Object.values(v.extensions).every(isEditorMode);
}

function isEditSession(v: unknown): v is EditSession {
  return isRecord(v) && isString(v.url) && isString(v.accessToken) && isNumber(v.accessTokenTtl);
}

function isCreated(v: unknown): v is { id: string; name: string } {
  return isRecord(v) && isString(v.id) && isString(v.name);
}

export async function fetchEditor(signal?: AbortSignal): Promise<EditorInfo> {
  return expectShape(await apiRequest("/editor", { signal }), isEditorInfo, "GET /editor").extensions;
}

export async function openEditor(id: string): Promise<EditSession> {
  const res = await apiRequest(`/nodes/${encodeURIComponent(id)}/edit`, { method: "POST" });
  return expectShape(res, isEditSession, "POST /nodes/{id}/edit");
}

// The editor's page, a full window of its own (see App).
export function editorPath(id: string): string {
  return `/edit/${encodeURIComponent(id)}`;
}

// Documents open in a tab of their own, so the file manager stays where it
// was. Called right in the click or key handler: browsers only let a page
// open a tab while it handles the user's gesture.
export function openEditorTab(id: string) {
  window.open(editorPath(id), "_blank", "noopener");
}

export async function createDocument(folderId: string, name: string): Promise<{ id: string; name: string }> {
  const res = await apiRequest(`/nodes/${encodeURIComponent(folderId)}/documents`, { method: "POST", body: { name } });
  return expectShape(res, isCreated, "POST /nodes/{id}/documents");
}

function extension(name: string): string {
  const dot = name.lastIndexOf(".");
  return dot > 0 ? name.slice(dot + 1).toLowerCase() : "";
}

// Office documents open in the editor on a double click. The editor also
// shows images, PDFs and plain text, but those the browser handles
// itself: they keep downloading.
const OFFICE = new Set([
  "doc", "docx", "docm", "dot", "dotx", "odt", "ott", "rtf", "fodt",
  "xls", "xlsx", "xlsm", "xlt", "xltx", "ods", "ots", "fods", "csv",
  "ppt", "pptx", "pptm", "pot", "potx", "odp", "otp", "fodp", "odg",
]);

// editorMode says how the editor opens a file, undefined when it doesn't.
export function editorMode(info: EditorInfo | undefined, name: string): EditorMode | undefined {
  const ext = extension(name);
  return info && OFFICE.has(ext) ? info[ext] : undefined;
}

// The kinds of new document offered, when the editor makes them.
export const NEW_DOCUMENTS = [
  { ext: "docx", label: "editor.newDocument" },
  { ext: "xlsx", label: "editor.newSpreadsheet" },
  { ext: "pptx", label: "editor.newPresentation" },
] as const;
