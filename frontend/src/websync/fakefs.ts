import type { DirHandle, FileHandle, Writable } from "./localfs";
import { nameKey } from "./names";

// An in-memory stand-in for a folder handle, for tests: the File System
// Access API's behaviour where the sync relies on it — NotFoundError for a
// missing name, a write that lands only on close(), removeEntry refusing a
// folder with something in it, and (optionally) a case-insensitive disk.

let clock = 1_000;

function notFound(name: string): DOMException {
  return new DOMException(`${name} not found`, "NotFoundError");
}

export class FakeFile implements FileHandle {
  readonly kind = "file";
  data: Uint8Array<ArrayBuffer>;
  lastModified: number;

  constructor(
    public name: string,
    public parent: FakeDir,
    data: Uint8Array<ArrayBuffer>,
  ) {
    this.data = data;
    this.lastModified = ++clock;
  }

  async getFile(): Promise<File> {
    return new File([this.data], this.name, { lastModified: this.lastModified });
  }

  async createWritable(): Promise<Writable> {
    const chunks: Uint8Array[] = [];
    return {
      write: async (d) => {
        chunks.push(d.slice());
      },
      close: async () => {
        const out = new Uint8Array(chunks.reduce((n, c) => n + c.length, 0));
        let off = 0;
        for (const c of chunks) {
          out.set(c, off);
          off += c.length;
        }
        this.data = out;
        this.lastModified = ++clock;
      },
      abort: async () => {
        chunks.length = 0;
      },
    };
  }

  // Set by FakeDir when the fake should have FileSystemHandle.move.
  move?: (destination: DirHandle, name: string) => Promise<void>;
}

export class FakeDir implements DirHandle {
  readonly kind = "directory";
  readonly children = new Map<string, FakeDir | FakeFile>();

  constructor(
    public name: string,
    readonly options: { caseInsensitive?: boolean; move?: boolean } = {},
  ) {}

  private find(name: string): FakeDir | FakeFile | undefined {
    if (!this.options.caseInsensitive) return this.children.get(name);
    const k = nameKey(name);
    for (const [n, c] of this.children) if (nameKey(n) === k) return c;
    return undefined;
  }

  async *values(): AsyncGenerator<FakeDir | FakeFile> {
    yield* [...this.children.values()];
  }

  async getDirectoryHandle(name: string, options?: { create?: boolean }): Promise<FakeDir> {
    const c = this.find(name);
    if (c instanceof FakeDir) return c;
    if (c) throw new DOMException(`${name} is a file`, "TypeMismatchError");
    if (!options?.create) throw notFound(name);
    const d = new FakeDir(name, this.options);
    this.children.set(name, d);
    return d;
  }

  async getFileHandle(name: string, options?: { create?: boolean }): Promise<FakeFile> {
    if (name.includes("\\") || name.includes("/")) throw new TypeError(`Name is not allowed: ${name}`);
    const c = this.find(name);
    if (c instanceof FakeFile) return c;
    if (c) throw new DOMException(`${name} is a folder`, "TypeMismatchError");
    if (!options?.create) throw notFound(name);
    return this.addFile(name, new Uint8Array(0));
  }

  addFile(name: string, data: Uint8Array<ArrayBuffer>): FakeFile {
    const f = new FakeFile(name, this, data);
    if (this.options.move) {
      f.move = async (destination, newName) => {
        if (!(destination instanceof FakeDir)) throw new TypeError("not a fake folder");
        f.parent.children.delete(f.name);
        f.name = newName;
        f.parent = destination;
        destination.children.set(newName, f);
      };
    }
    this.children.set(name, f);
    return f;
  }

  async removeEntry(name: string, options?: { recursive?: boolean }): Promise<void> {
    const c = this.find(name);
    if (!c) throw notFound(name);
    if (c instanceof FakeDir && c.children.size > 0 && !options?.recursive) throw new DOMException(`${name} is not empty`, "InvalidModificationError");
    this.children.delete(c.name);
  }

  async isSameEntry(other: DirHandle): Promise<boolean> {
    return other === this;
  }

  async queryPermission(): Promise<PermissionState> {
    return "granted";
  }
}

// fakeFolder builds a folder from {"path": "content"}; a path ending in
// "/" is an empty folder.
export function fakeFolder(files: Record<string, string>, options: { caseInsensitive?: boolean; move?: boolean } = {}): FakeDir {
  const root = new FakeDir("root", options);
  for (const [path, content] of Object.entries(files)) {
    const parts = path.split("/");
    let d = root;
    for (const p of parts.slice(0, -1)) {
      const c = d.children.get(p);
      if (c instanceof FakeDir) d = c;
      else {
        const n = new FakeDir(p, options);
        d.children.set(p, n);
        d = n;
      }
    }
    const last = parts[parts.length - 1];
    if (last !== "") d.addFile(last, new TextEncoder().encode(content));
  }
  return root;
}

// contents reads a folder back as {"path": "content"}, empty folders as
// "path/".
export function contents(dir: FakeDir, prefix = ""): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [name, c] of [...dir.children].sort(([a], [b]) => (a < b ? -1 : 1))) {
    const p = prefix + name;
    if (c instanceof FakeFile) out[p] = new TextDecoder().decode(c.data);
    else if (c.children.size === 0) out[`${p}/`] = "";
    else Object.assign(out, contents(c, `${p}/`));
  }
  return out;
}
