// Incremental SHA-256 (FIPS 180-4). Web Crypto only digests a whole buffer
// at once, and a synced file can be gigabytes: hashing it piece by piece is
// what keeps a big file out of memory, both when reading it from disk and
// while a download streams through.

const K = new Uint32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5, 0xd807aa98, 0x12835b01, 0x243185be,
  0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174, 0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa,
  0x5cb0a9dc, 0x76f988da, 0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967, 0x27b70a85,
  0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85, 0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3,
  0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070, 0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f,
  0x682e6ff3, 0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]);

export class Sha256 {
  private readonly h = new Uint32Array([0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19]);
  private readonly w = new Uint32Array(64);
  // Bytes of an incomplete block, waiting for the next update.
  private readonly buf = new Uint8Array(64);
  private bufLen = 0;
  // Total length in bytes; a double holds it exactly up to 2^53.
  private length = 0;
  private finished = false;

  update(data: Uint8Array): this {
    if (this.finished) throw new Error("sha256: update after digest");
    this.length += data.length;
    let off = 0;
    if (this.bufLen > 0) {
      const take = Math.min(64 - this.bufLen, data.length);
      this.buf.set(data.subarray(0, take), this.bufLen);
      this.bufLen += take;
      off = take;
      if (this.bufLen < 64) return this;
      this.block(this.buf, 0);
      this.bufLen = 0;
    }
    for (; off + 64 <= data.length; off += 64) this.block(data, off);
    if (off < data.length) {
      this.buf.set(data.subarray(off), 0);
      this.bufLen = data.length - off;
    }
    return this;
  }

  digest(): Uint8Array {
    if (this.finished) throw new Error("sha256: digest twice");
    this.finished = true;
    const bits = this.length * 8;
    const pad = new Uint8Array(this.bufLen < 56 ? 64 : 128);
    pad.set(this.buf.subarray(0, this.bufLen));
    pad[this.bufLen] = 0x80;
    // The length goes in big-endian at the end, high word first.
    const view = new DataView(pad.buffer);
    view.setUint32(pad.length - 8, Math.floor(bits / 0x100000000));
    view.setUint32(pad.length - 4, bits >>> 0);
    for (let off = 0; off < pad.length; off += 64) this.block(pad, off);
    const out = new Uint8Array(32);
    const ov = new DataView(out.buffer);
    for (let i = 0; i < 8; i++) ov.setUint32(i * 4, this.h[i]);
    return out;
  }

  hex(): string {
    let s = "";
    for (const b of this.digest()) s += b.toString(16).padStart(2, "0");
    return s;
  }

  private block(p: Uint8Array, off: number) {
    const w = this.w;
    for (let i = 0; i < 16; i++) {
      const j = off + i * 4;
      w[i] = (p[j] << 24) | (p[j + 1] << 16) | (p[j + 2] << 8) | p[j + 3];
    }
    for (let i = 16; i < 64; i++) {
      const a = w[i - 15];
      const b = w[i - 2];
      const s0 = ((a >>> 7) | (a << 25)) ^ ((a >>> 18) | (a << 14)) ^ (a >>> 3);
      const s1 = ((b >>> 17) | (b << 15)) ^ ((b >>> 19) | (b << 13)) ^ (b >>> 10);
      w[i] = (w[i - 16] + s0 + w[i - 7] + s1) | 0;
    }
    const h = this.h;
    let a = h[0];
    let b = h[1];
    let c = h[2];
    let d = h[3];
    let e = h[4];
    let f = h[5];
    let g = h[6];
    let hh = h[7];
    for (let i = 0; i < 64; i++) {
      const S1 = ((e >>> 6) | (e << 26)) ^ ((e >>> 11) | (e << 21)) ^ ((e >>> 25) | (e << 7));
      const ch = (e & f) ^ (~e & g);
      const t1 = (hh + S1 + ch + K[i] + w[i]) | 0;
      const S0 = ((a >>> 2) | (a << 30)) ^ ((a >>> 13) | (a << 19)) ^ ((a >>> 22) | (a << 10));
      const maj = (a & b) ^ (a & c) ^ (b & c);
      const t2 = (S0 + maj) | 0;
      hh = g;
      g = f;
      f = e;
      e = (d + t1) | 0;
      d = c;
      c = b;
      b = a;
      a = (t1 + t2) | 0;
    }
    h[0] += a;
    h[1] += b;
    h[2] += c;
    h[3] += d;
    h[4] += e;
    h[5] += f;
    h[6] += g;
    h[7] += hh;
  }
}

// How much of a file is read at a time: big enough that the per-read cost
// doesn't matter, small enough to stay out of the way of the page.
export const HASH_CHUNK = 4 * 1024 * 1024;

// Up to this size Web Crypto (native, several times faster) hashes the
// whole file in memory.
const SUBTLE_MAX = 8 * 1024 * 1024;

function toHex(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += b.toString(16).padStart(2, "0");
  return s;
}

// Hex SHA-256 of a file's content, read in HASH_CHUNK slices; signal stops
// it between slices.
export async function hashFile(file: Blob, signal?: AbortSignal, onProgress?: (done: number) => void): Promise<string> {
  const subtle = globalThis.crypto?.subtle;
  if (subtle && file.size <= SUBTLE_MAX) {
    const sum = await subtle.digest("SHA-256", await file.arrayBuffer());
    onProgress?.(file.size);
    return toHex(new Uint8Array(sum));
  }
  const h = new Sha256();
  for (let off = 0; off < file.size; off += HASH_CHUNK) {
    signal?.throwIfAborted();
    h.update(new Uint8Array(await file.slice(off, off + HASH_CHUNK).arrayBuffer()));
    onProgress?.(Math.min(off + HASH_CHUNK, file.size));
  }
  return h.hex();
}
