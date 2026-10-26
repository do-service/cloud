import { useQueryClient } from "@tanstack/react-query";
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { DetailedError, Upload } from "tus-js-client";
import { ApiError } from "./api";
import { createFolder, fetchChildren, type FileNode } from "./files";
import { isRecord, isString } from "./guards";

export const UPLOAD_ENDPOINT = "/api/v1/uploads/";
// How many files go up at once: enough to fill the link with small files,
// few enough that one big file isn't starved.
export const PARALLEL_UPLOADS = 3;
// Bounds what a dropped connection costs, and keeps every request under
// the body limits of the reverse proxies people put in front.
export const CHUNK_SIZE = 64 * 1024 * 1024;
// Network trouble is retried from the last confirmed offset (tus HEAD).
export const RETRY_DELAYS = [0, 1000, 3000, 5000, 10_000, 20_000, 30_000];

// A file to upload and the folders it sits in, relative to the drop target:
// "Photos/2026/a.jpg" puts a.jpg into Photos/2026, made as needed.
export type PickedFile = { file: File; path: string };

// conflict: the name is taken in the folder, and the user picks what to do.
export type UploadStatus = "queued" | "uploading" | "done" | "error" | "canceled" | "conflict";

// What to do about a taken name: ask first (fail), add a version (replace),
// or keep both (rename: "name (2)").
export type ConflictPolicy = "fail" | "replace" | "rename";

// A user's answer to a conflict.
export type ConflictChoice = "replace" | "rename" | "skip";

export type UploadTask = {
  key: number;
  name: string;
  // Folders it goes into below the target, "" for the target itself.
  dir: string;
  size: number;
  sent: number;
  status: UploadStatus;
  error?: unknown;
};

type Job = { task: UploadTask; file: File; parentId: string; dir: string; policy: ConflictPolicy; upload?: Upload };

type Uploads = {
  tasks: UploadTask[];
  enqueue: (files: PickedFile[], parentId: string) => void;
  cancel: (key: number) => void;
  retry: (key: number) => void;
  // Answers the conflict of one upload, or (key null) of all of them.
  resolve: (key: number | null, choice: ConflictChoice) => void;
  clearFinished: () => void;
};

const UploadsContext = createContext<Uploads | null>(null);

export function useUploads(): Uploads {
  const ctx = useContext(UploadsContext);
  if (!ctx) throw new Error("useUploads outside UploadsProvider");
  return ctx;
}

function dirOf(path: string): string {
  const i = path.lastIndexOf("/");
  return i < 0 ? "" : path.slice(0, i);
}

function isNameConflict(err: unknown): boolean {
  return err instanceof ApiError && err.code === "name_conflict";
}

// The API error behind a failed tus request, so errorText can explain it:
// tus-js-client wraps the response, ours carries {"code","message"}.
export function uploadError(err: unknown): unknown {
  if (err instanceof DetailedError && err.originalResponse) {
    const res = err.originalResponse;
    try {
      const body: unknown = JSON.parse(res.getBody());
      if (isRecord(body) && isString(body.code)) return new ApiError(res.getStatus(), body.code, isString(body.message) ? body.message : "");
    } catch {
      // Not our JSON: a proxy's page, say.
    }
    return new ApiError(res.getStatus(), "unknown", err.message);
  }
  if (err instanceof DetailedError) return new ApiError(0, "network", err.message);
  return err;
}

// The id of the folder name in parentId, made if missing. An existing one
// is used, so dropping "Photos" next to a "Photos" adds to it.
export async function ensureFolder(parentId: string, name: string): Promise<string> {
  try {
    return (await createFolder(parentId, name)).id;
  } catch (err) {
    if (!(err instanceof ApiError && err.code === "name_conflict")) throw err;
  }
  // Folders come first in a listing, so this stops early. Names match the
  // way the backend's do: case-insensitively.
  const want = name.normalize("NFC").toLowerCase();
  let cursor: string | null = null;
  do {
    const page = await fetchChildren(parentId, { key: "name", order: "asc" }, cursor);
    const found = page.items.find((n: FileNode) => n.kind === "dir" && n.name.normalize("NFC").toLowerCase() === want);
    if (found) return found.id;
    if (page.items.some((n) => n.kind === "file")) break;
    cursor = page.nextCursor;
  } while (cursor);
  // Taken by a file: a folder can't go there.
  throw new ApiError(409, "name_conflict", `a file named ${name} is in the way`);
}

// Turns dropped items into files with their paths, walking into folders.
export async function filesFromDrop(items: DataTransferItemList): Promise<PickedFile[]> {
  const entries = Array.from(items)
    .map((item) => (item.kind === "file" ? item.webkitGetAsEntry() : null))
    .filter((e): e is FileSystemEntry => e !== null);
  const out: PickedFile[] = [];
  const walk = async (entry: FileSystemEntry, prefix: string): Promise<void> => {
    if (isFileEntry(entry)) {
      const file = await new Promise<File>((resolve, reject) => entry.file(resolve, reject));
      out.push({ file, path: prefix + file.name });
    } else if (isDirectoryEntry(entry)) {
      const reader = entry.createReader();
      // readEntries hands out a directory in batches until an empty one.
      for (;;) {
        const batch = await new Promise<FileSystemEntry[]>((resolve, reject) => reader.readEntries(resolve, reject));
        if (batch.length === 0) break;
        for (const child of batch) await walk(child, `${prefix}${entry.name}/`);
      }
    }
  };
  for (const entry of entries) await walk(entry, "");
  return out;
}

function isFileEntry(e: FileSystemEntry): e is FileSystemFileEntry {
  return e.isFile;
}

function isDirectoryEntry(e: FileSystemEntry): e is FileSystemDirectoryEntry {
  return e.isDirectory;
}

// Files from an <input type=file>, with folder paths when it picked a
// directory (webkitdirectory).
export function filesFromInput(list: FileList): PickedFile[] {
  return Array.from(list).map((file) => ({ file, path: file.webkitRelativePath || file.name }));
}

// Owns every upload of the session: a queue run PARALLEL_UPLOADS at a time
// over tus, folders made on the way, listings refreshed as files land.
export function UploadsProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  const [tasks, setTasks] = useState<UploadTask[]>([]);
  const jobs = useRef(new Map<number, Job>());
  const nextKey = useRef(1);
  // One mkdir per folder path and target, shared by the files inside it.
  const folders = useRef(new Map<string, Promise<string>>());

  const update = useCallback((key: number, patch: Partial<UploadTask>) => {
    setTasks((all) => all.map((t) => (t.key === key ? { ...t, ...patch } : t)));
    const job = jobs.current.get(key);
    if (job) job.task = { ...job.task, ...patch };
  }, []);

  const folderFor = useCallback((parentId: string, dir: string): Promise<string> => {
    if (dir === "") return Promise.resolve(parentId);
    const memo = `${parentId}:${dir}`;
    let p = folders.current.get(memo);
    if (!p) {
      p = folderFor(parentId, dirOf(dir)).then((parent) => ensureFolder(parent, dir.slice(dir.lastIndexOf("/") + 1)));
      // A failure isn't cached: a retry tries again.
      p.catch(() => folders.current.delete(memo));
      folders.current.set(memo, p);
    }
    return p;
  }, []);

  const refresh = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: ["children"] });
    void queryClient.invalidateQueries({ queryKey: ["node"] });
  }, [queryClient]);

  const start = useCallback(
    async (job: Job) => {
      const key = job.task.key;
      // update() marks the job synchronously, so a second call for it (the
      // effect below runs twice under StrictMode) stops here.
      if (job.task.status !== "queued") return;
      update(key, { status: "uploading", error: undefined });
      let parentId: string;
      try {
        parentId = await folderFor(job.parentId, job.dir);
      } catch (err) {
        update(key, { status: "error", error: err });
        return;
      }
      if (jobs.current.get(key)?.task.status !== "uploading") return; // canceled meanwhile
      const upload = new Upload(job.file, {
        endpoint: UPLOAD_ENDPOINT,
        chunkSize: CHUNK_SIZE,
        retryDelays: RETRY_DELAYS,
        metadata: { filename: job.file.name, parentId, onConflict: job.policy },
        // Kept in localStorage so picking the same file again after a
        // reload resumes it instead of starting over.
        storeFingerprintForResuming: true,
        removeFingerprintOnSuccess: true,
        // A 4xx other than a lost lock won't get better by retrying.
        onShouldRetry: (err) => {
          const status = err.originalResponse?.getStatus() ?? 0;
          // A 409 for a wrong offset is retried from a fresh HEAD; a taken
          // name is the user's to decide.
          return (status === 0 || status === 409 || status === 423 || status >= 500) && !isNameConflict(uploadError(err));
        },
        onProgress: (sent) => update(key, { sent }),
        onSuccess: () => {
          update(key, { status: "done", sent: job.file.size });
          refresh();
        },
        onError: (err) => {
          const cause = uploadError(err);
          update(key, isNameConflict(cause) ? { status: "conflict" } : { status: "error", error: cause });
        },
      });
      job.upload = upload;
      const previous = await upload.findPreviousUploads();
      if (previous.length > 0) upload.resumeFromPreviousUpload(previous[0]);
      upload.start();
    },
    [folderFor, refresh, update],
  );

  // Starts queued jobs while fewer than PARALLEL_UPLOADS are running.
  useEffect(() => {
    const running = tasks.filter((t) => t.status === "uploading").length;
    const waiting = tasks.filter((t) => t.status === "queued").slice(0, Math.max(0, PARALLEL_UPLOADS - running));
    for (const t of waiting) {
      const job = jobs.current.get(t.key);
      if (job) void start(job);
    }
  }, [tasks, start]);

  // Leaving the page would stop the uploads; the browser asks first.
  const active = tasks.some((t) => t.status === "uploading" || t.status === "queued");
  useEffect(() => {
    if (!active) return;
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [active]);

  const enqueue = useCallback((files: PickedFile[], parentId: string) => {
    const added = files.map(({ file, path }) => {
      const task: UploadTask = { key: nextKey.current++, name: file.name, dir: dirOf(path), size: file.size, sent: 0, status: "queued" };
      jobs.current.set(task.key, { task, file, parentId, dir: task.dir, policy: "fail" });
      return task;
    });
    setTasks((all) => [...all, ...added]);
  }, []);

  const cancel = useCallback(
    (key: number) => {
      const job = jobs.current.get(key);
      // abort(true) also deletes the upload on the server, ending its
      // quota reservation.
      void job?.upload?.abort(true).catch(() => undefined);
      update(key, { status: "canceled" });
    },
    [update],
  );

  const retry = useCallback((key: number) => update(key, { status: "queued", error: undefined }), [update]);

  const resolve = useCallback(
    (key: number | null, choice: ConflictChoice) => {
      for (const job of jobs.current.values()) {
        if (job.task.status !== "conflict" || (key !== null && job.task.key !== key)) continue;
        if (choice === "skip") {
          update(job.task.key, { status: "canceled" });
        } else {
          job.policy = choice;
          update(job.task.key, { status: "queued" });
        }
      }
    },
    [update],
  );

  const clearFinished = useCallback(() => {
    setTasks((all) => {
      const keep = all.filter((t) => t.status === "uploading" || t.status === "queued" || t.status === "error" || t.status === "conflict");
      for (const t of all) if (!keep.includes(t)) jobs.current.delete(t.key);
      return keep;
    });
  }, []);

  const value = useMemo(
    () => ({ tasks, enqueue, cancel, retry, resolve, clearFinished }),
    [tasks, enqueue, cancel, retry, resolve, clearFinished],
  );
  return <UploadsContext.Provider value={value}>{children}</UploadsContext.Provider>;
}
