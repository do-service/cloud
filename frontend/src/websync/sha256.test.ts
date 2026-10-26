import { describe, expect, it } from "vitest";
import { HASH_CHUNK, Sha256, hashFile } from "./sha256";

const text = (s: string) => new TextEncoder().encode(s);
const hex = (data: Uint8Array) => new Sha256().update(data).hex();

async function subtleHex(data: Uint8Array<ArrayBuffer>): Promise<string> {
  const sum = new Uint8Array(await crypto.subtle.digest("SHA-256", data));
  return Array.from(sum, (b) => b.toString(16).padStart(2, "0")).join("");
}

// A deterministic byte pattern: the same on every run, no two blocks alike.
function bytes(n: number): Uint8Array<ArrayBuffer> {
  const out = new Uint8Array(n);
  let x = 0x12345678;
  for (let i = 0; i < n; i++) {
    x = (x * 1103515245 + 12345) >>> 0;
    out[i] = x >>> 24;
  }
  return out;
}

describe("Sha256", () => {
  // FIPS 180-4 examples and the NIST CAVS long-message vector.
  it.each([
    ["", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"],
    ["abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"],
    ["abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq", "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1"],
    [
      "abcdefghbcdefghicdefghijdefghijkefghijklfghijklmghijklmnhijklmnoijklmnopjklmnopqklmnopqrlmnopqrsmnopqrstnopqrstu",
      "cf5b16a778af8380036ce59e7b0492370b249b11e8f07a51afac45037afee9d1",
    ],
  ])("hashes %j", (input, want) => {
    expect(hex(text(input))).toBe(want);
  });

  it("hashes a million a's fed in uneven pieces", () => {
    const h = new Sha256();
    const piece = text("a".repeat(997));
    let left = 1_000_000;
    while (left > 0) {
      const n = Math.min(left, piece.length);
      h.update(piece.subarray(0, n));
      left -= n;
    }
    expect(h.hex()).toBe("cdc76e5c9914fb9281a1c7e284d73e67f1809a48a497200e046d39ccc7112cd0");
  });

  it("matches Web Crypto at every length around the padding and block edges", async () => {
    for (let n = 0; n <= 200; n++) {
      const data = bytes(n);
      expect(hex(data), `length ${n}`).toBe(await subtleHex(data));
    }
  });

  it("gives the same digest however the input is split", async () => {
    const data = bytes(300);
    const want = await subtleHex(data);
    for (let cut = 0; cut <= data.length; cut += 7) {
      for (const second of [1, 63, 64, 65]) {
        const h = new Sha256();
        h.update(data.subarray(0, cut));
        h.update(data.subarray(cut, cut + second));
        h.update(data.subarray(cut + second));
        expect(h.hex(), `cuts at ${cut} and ${cut + second}`).toBe(want);
      }
    }
  });

  it("refuses to be used after its digest", () => {
    const h = new Sha256();
    h.digest();
    expect(() => h.update(text("x"))).toThrow();
    expect(() => h.digest()).toThrow();
  });
});

describe("hashFile", () => {
  it("hashes a file bigger than a read chunk in slices", async () => {
    const data = bytes(HASH_CHUNK * 2 + 12345);
    const seen: number[] = [];
    const got = await hashFile(new Blob([data]), undefined, (done) => seen.push(done));
    expect(got).toBe(await subtleHex(data));
    expect(seen).toEqual([HASH_CHUNK, HASH_CHUNK * 2, data.length]);
  });

  it("hashes a small file", async () => {
    expect(await hashFile(new Blob([text("abc")]))).toBe("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
  });

  it("stops when aborted", async () => {
    const ctl = new AbortController();
    ctl.abort();
    await expect(hashFile(new Blob([bytes(HASH_CHUNK * 3)]), ctl.signal)).rejects.toThrow();
  });
});
