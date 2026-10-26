import type { FileNode } from "./files";
import { t } from "./i18n";
import { extension } from "./preview";

// What a file is, for its icon and the name of its type: by MIME where it
// says, by extension where the server only knows octet-stream (a type
// table without the name's extension).
export type FileType =
  | "folder"
  | "image"
  | "video"
  | "audio"
  | "pdf"
  | "document"
  | "spreadsheet"
  | "presentation"
  | "text"
  | "archive"
  | "other";

const BY_EXTENSION: Record<string, FileType> = {
  pdf: "pdf",
  doc: "document",
  docx: "document",
  odt: "document",
  rtf: "document",
  xls: "spreadsheet",
  xlsx: "spreadsheet",
  ods: "spreadsheet",
  csv: "spreadsheet",
  ppt: "presentation",
  pptx: "presentation",
  odp: "presentation",
  zip: "archive",
  "7z": "archive",
  rar: "archive",
  tar: "archive",
  gz: "archive",
  tgz: "archive",
  txt: "text",
  md: "text",
};

const BY_MIME: [RegExp, FileType][] = [
  [/^image\//, "image"],
  [/^video\//, "video"],
  [/^audio\//, "audio"],
  [/^application\/pdf$/, "pdf"],
  [/wordprocessingml|msword|opendocument\.text|rtf/, "document"],
  [/spreadsheetml|ms-excel|opendocument\.spreadsheet|^text\/csv$/, "spreadsheet"],
  [/presentationml|ms-powerpoint|opendocument\.presentation/, "presentation"],
  [/zip|x-7z|x-rar|x-tar|gzip/, "archive"],
  [/^text\//, "text"],
];

export function fileType(n: Pick<FileNode, "kind" | "name" | "mime">): FileType {
  if (n.kind === "dir") return "folder";
  const mime = (n.mime ?? "").split(";")[0].trim().toLowerCase();
  // An office file is a zip inside: its extension says more than a
  // generic archive MIME would.
  const byExt = BY_EXTENSION[extension(n.name)];
  if (byExt && byExt !== "archive" && byExt !== "text") return byExt;
  for (const [re, type] of BY_MIME) if (re.test(mime)) return type;
  return byExt ?? "other";
}

// typeLabel names a node's type for people: «Изображение PNG», «Папка».
export function typeLabel(n: Pick<FileNode, "kind" | "name" | "mime">): string {
  const type = fileType(n);
  if (type === "folder") return t("files.types.folder");
  const ext = extension(n.name).toUpperCase();
  if (ext === "") return t(`files.types.${type}`);
  return t(`files.types.${type}Ext`, { ext });
}
