// Names as the server and the disk see them. Paths here are relative to
// the chosen local folder, "/"-separated, "" for the folder itself.

// Lower-cases one code point the way Go's unicode.ToLower does (the
// server's strings.ToLower): by the simple case mapping only. JavaScript's
// toLowerCase also applies the special ones — İ becomes "i̇", two code
// points, and a final Σ becomes ς — which would give the same name another
// key than the server's.
function lowerRune(ch: string): string {
  if (ch === "İ") return "i";
  const l = ch.toLowerCase();
  return l.length === ch.length || [...l].length === 1 ? l : ch;
}

// nameKey is how the server compares names (backend/internal/fs/names.go):
// lower-cased NFC. Two names with one key are one name to the server, and
// to a case-insensitive disk (macOS and Windows by default).
export function nameKey(name: string): string {
  const n = name.normalize("NFC");
  // Plain ASCII, the usual case, has no special mappings.
  if (!/[^\x00-\x7f]/.test(n)) return n.toLowerCase();
  let out = "";
  for (const ch of n) out += lowerRune(ch);
  return out;
}

// pathKey is nameKey of every component: what a path is matched by.
export function pathKey(path: string): string {
  return path === "" ? "" : path.split("/").map(nameKey).join("/");
}

const encoder = new TextEncoder();

function utf8Length(s: string): number {
  return encoder.encode(s).length;
}

// validName reports whether the server takes name (fs.NormalizeName): not
// empty, "." or "..", at most 255 bytes of UTF-8 in NFC, no "/" and no
// control characters below U+0020.
export function validName(name: string): boolean {
  const n = name.normalize("NFC");
  if (n === "" || n === "." || n === ".." || utf8Length(n) > 255 || n.includes("/")) return false;
  // A lone surrogate is invalid UTF-8 to the server.
  if (/[\ud800-\udbff](?![\udc00-\udfff])|(?<![\ud800-\udbff])[\udc00-\udfff]/.test(n)) return false;
  return !/[\x00-\x1f]/.test(n);
}

function reservedWindows(stem: string): boolean {
  const s = stem.toUpperCase();
  if (["CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$"].includes(s)) return true;
  return /^(COM|LPT)[0-9¹²³]$/.test(s);
}

// localNameAllowed reports whether a name can be created in the local
// folder: never with a backslash (the browser refuses it as a path
// separator), and on Windows none of what Windows can't store — <>:"|?*,
// a trailing dot or space, a reserved device name (as the desktop client's
// localfs.CheckWindowsName).
export function localNameAllowed(name: string, windows: boolean): boolean {
  if (name.includes("\\")) return false;
  if (!windows) return true;
  if (/[<>:\x22|?*\x00-\x1f]/.test(name) || name.endsWith(".") || name.endsWith(" ")) return false;
  return !reservedWindows(name.split(".")[0].trimEnd());
}

// This sync's own temporary files start with it (as the desktop client's).
export const TEMP_PREFIX = ".~cloud-";

// isJunk: the OS's metadata, which appears in any folder a file manager
// opened — safe to delete with the folder, unlike a lock file, which means
// a document in it is open.
export function isJunk(name: string): boolean {
  const lower = name.toLowerCase();
  return name === ".DS_Store" || name === "Icon\r" || lower === "thumbs.db" || lower === "desktop.ini";
}

// isIgnored: left out of sync on both sides, wherever it is — OS metadata,
// the lock and temporary files editors and browsers keep next to a document
// (the desktop client's localfs.Ignored), and Chrome's own swap file of a
// file being written through this API (.crswap).
export function isIgnored(name: string): boolean {
  if (isJunk(name) || name === ".cloudsync" || name.startsWith(TEMP_PREFIX)) return true;
  const lower = name.toLowerCase();
  if (name.startsWith("~$") || (name.startsWith(".~lock.") && name.endsWith("#"))) return true;
  return [".tmp", ".crdownload", ".part", ".crswap"].some((ext) => lower.endsWith(ext));
}

function truncateUtf8(s: string, max: number): string {
  if (utf8Length(s) <= max) return s;
  let out = "";
  for (const ch of s) {
    if (utf8Length(out + ch) > max) break;
    out += ch;
  }
  return out;
}

function pad(n: number): string {
  return String(n).padStart(2, "0");
}

// The date in a conflict copy's name, local time: ГГГГ-ММ-ДД ЧЧ-ММ.
export function conflictStamp(at: Date): string {
  return `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())} ${pad(at.getHours())}-${pad(at.getMinutes())}`;
}

// conflictName is the desktop client's conflict copy name (engine/names.go):
// tag is the «(конфликт, Устройство, ГГГГ-ММ-ДД ЧЧ-ММ)» part, made by the
// caller from the dictionary; it goes before the last extension (a dotfile
// has none). The stem is shortened to keep the name within the server's
// 255 bytes.
export function conflictName(name: string, tag: string): string {
  const n = name.normalize("NFC");
  const dot = n.lastIndexOf(".");
  const [stem, ext] = dot > 0 ? [n.slice(0, dot), n.slice(dot)] : [n, ""];
  const room = 255 - utf8Length(` ${tag}`) - utf8Length(ext);
  return `${truncateUtf8(stem, Math.max(room, 1))} ${tag}${ext}`;
}

export function dirOf(path: string): string {
  const i = path.lastIndexOf("/");
  return i < 0 ? "" : path.slice(0, i);
}

export function baseName(path: string): string {
  return path.slice(path.lastIndexOf("/") + 1);
}

export function join(dir: string, name: string): string {
  return dir === "" ? name : `${dir}/${name}`;
}

// under: path is dir or inside it; everything is under "".
export function under(path: string, dir: string): boolean {
  return dir === "" || path === dir || path.startsWith(`${dir}/`);
}

export function depth(path: string): number {
  return path === "" ? 0 : path.split("/").length;
}
