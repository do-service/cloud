import { ApiError, toApiError } from "./api";
import type { FileNode } from "./files";

// Previews (R6, E9-5): which files have them, and where they are. The
// list mirrors backend/internal/preview's; the server has the last word,
// a file it can't preview just shows its icon.

const PREVIEWED = new Set(["jpg", "jpeg", "png", "webp", "gif", "bmp", "heic", "heif", "tif", "tiff", "pdf"]);
const MAX_SOURCE = 200 * 1024 * 1024;

export type PreviewSize = "s" | "l";

export function extension(name: string): string {
  const dot = name.lastIndexOf(".");
  return dot > 0 ? name.slice(dot + 1).toLowerCase() : "";
}

export function previewable(n: FileNode): boolean {
  return n.kind === "file" && n.size > 0 && n.size <= MAX_SOURCE && PREVIEWED.has(extension(n.name));
}

// What a file shows as from its server preview: an image (its large
// preview, which the browser can show whatever the format), a PDF (the
// browser's own viewer), or nothing.
export function mediaKind(n: FileNode): "image" | "pdf" | undefined {
  if (!previewable(n)) return undefined;
  return extension(n.name) === "pdf" ? "pdf" : "image";
}

// Shown as text by the viewer: read by the client, never by the server's
// preview, so any size goes (the viewer reads only the start).
const TEXT = new Set([
  "txt", "md", "markdown", "log", "csv", "tsv", "json", "xml", "yaml", "yml", "toml", "ini", "conf", "cfg", "env",
  "sh", "bash", "zsh", "ps1", "bat", "js", "mjs", "cjs", "ts", "tsx", "jsx", "css", "scss", "html", "htm",
  "go", "py", "rb", "rs", "java", "kt", "c", "h", "cpp", "hpp", "cs", "php", "sql", "swift", "dart", "lua", "r", "pl",
  "vue", "svelte",
]);
// Text files that go by name rather than extension.
const TEXT_NAMES = new Set(["dockerfile", "makefile", "readme", "license", "changelog"]);

export function isText(n: FileNode): boolean {
  if (n.kind !== "file") return false;
  // Nothing to misread in an empty file: the viewer says it's empty.
  if (n.size === 0) return true;
  const ext = extension(n.name);
  return (n.mime ?? "").startsWith("text/") || TEXT.has(ext) || (ext === "" && TEXT_NAMES.has(n.name.toLowerCase()));
}

export type ViewerKind = "image" | "pdf" | "text";

// What the viewer shows a file as, or nothing (it's downloaded instead).
export function viewerKind(n: FileNode): ViewerKind | undefined {
  return mediaKind(n) ?? (isText(n) ? "text" : undefined);
}

// The preview URL changes with the content (?v=), so the browser keeps it
// as long as the content lives.
export function previewUrl(id: string, size: PreviewSize, etag: string): string {
  return `/api/v1/nodes/${encodeURIComponent(id)}/preview?${new URLSearchParams({ size, v: etag })}`;
}

// A file the browser can show safely, shown rather than downloaded.
export function inlineUrl(id: string): string {
  return `/api/v1/nodes/${encodeURIComponent(id)}/content?inline=1`;
}

// How much the viewer reads of a text file, and the preview panel of it.
export const TEXT_LIMIT = 1024 * 1024;
export const TEXT_PEEK = 4096;

export type TextHead = { text: string; truncated: boolean };

export function textQueryKey(url: string, etag: string, limit: number) {
  return ["text", url, etag, limit] as const;
}

// fetchTextHead reads the first limit bytes of a file as UTF-8: a range
// request, so a log of gigabytes costs no more than a short note.
export async function fetchTextHead(url: string, limit: number, signal?: AbortSignal): Promise<TextHead> {
  const label = `GET ${url}`;
  let res: Response;
  try {
    res = await fetch(url, { signal, credentials: "same-origin", headers: { Range: `bytes=0-${limit - 1}` } });
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") throw err;
    throw new ApiError(0, "network", `${label}: ${err instanceof Error ? err.message : String(err)}`);
  }
  if (!res.ok) throw await toApiError(res, label);
  const bytes = new Uint8Array(await res.arrayBuffer());
  // A server that ignored the range sent it all (200): the length says.
  const total = Number(res.headers.get("Content-Range")?.split("/")[1] ?? bytes.length);
  const truncated = total > limit || bytes.length > limit;
  const head = bytes.subarray(0, Math.min(limit, bytes.length));
  // Not fatal: a binary or mis-encoded file still shows, with U+FFFD.
  const text = new TextDecoder("utf-8").decode(truncated ? head.subarray(0, wholeChars(head)) : head);
  return { text, truncated };
}

// wholeChars is where UTF-8 text cut at an arbitrary byte ends in whole
// characters: a sequence split by the cut is dropped, not shown as U+FFFD.
function wholeChars(b: Uint8Array): number {
  // A character is at most 4 bytes: its lead byte is among the last 4.
  for (let i = b.length - 1; i >= Math.max(0, b.length - 4); i--) {
    const c = b[i];
    if ((c & 0xc0) === 0x80) continue;
    const size = c >= 0xf0 ? 4 : c >= 0xe0 ? 3 : c >= 0xc0 ? 2 : 1;
    return i + size > b.length ? i : b.length;
  }
  return b.length;
}
