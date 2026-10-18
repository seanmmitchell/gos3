// Small hashing helpers used for request body checksums.
// S3 requires Content-MD5 (or an x-amz-checksum-*) on DeleteObjects and most
// Put*Configuration calls; WebCrypto has no MD5, so it is implemented here.

import { sha256 } from '@noble/hashes/sha2.js'

const encoder = new TextEncoder()

export type BytesInput = string | ArrayBuffer | ArrayBufferView

/** Converts input to bytes (strings are UTF-8 encoded). */
export function toBytes(input: BytesInput): Uint8Array {
  if (typeof input === 'string') return encoder.encode(input)
  if (input instanceof ArrayBuffer) return new Uint8Array(input)
  return new Uint8Array(input.buffer, input.byteOffset, input.byteLength)
}

/** Standard base64 of bytes. */
export function bytesToBase64(bytes: Uint8Array): string {
  let bin = ''
  const chunk = 0x8000
  for (let i = 0; i < bytes.length; i += chunk) {
    bin += String.fromCharCode(...bytes.subarray(i, i + chunk))
  }
  return btoa(bin)
}

/** Lower-case hex of bytes. */
export function bytesToHex(bytes: Uint8Array): string {
  let out = ''
  for (const b of bytes) out += b.toString(16).padStart(2, '0')
  return out
}

// ---------------------------------------------------------------------------
// MD5 (RFC 1321)
// ---------------------------------------------------------------------------

const S = [
  7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22,
  5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20,
  4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23,
  6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21
]

const K = new Uint32Array(64)
for (let i = 0; i < 64; i++) K[i] = Math.floor(Math.abs(Math.sin(i + 1)) * 2 ** 32) >>> 0

/** MD5 digest (16 bytes). */
export function md5(input: BytesInput): Uint8Array {
  const msg = toBytes(input)
  const len = msg.length
  const padded = new Uint8Array(((len + 8) >>> 6) * 64 + 64)
  padded.set(msg)
  padded[len] = 0x80
  const bitLen = len * 8
  const view = new DataView(padded.buffer)
  view.setUint32(padded.length - 8, bitLen >>> 0, true)
  view.setUint32(padded.length - 4, Math.floor(bitLen / 2 ** 32) >>> 0, true)

  let a0 = 0x67452301
  let b0 = 0xefcdab89
  let c0 = 0x98badcfe
  let d0 = 0x10325476
  const M = new Uint32Array(16)

  for (let off = 0; off < padded.length; off += 64) {
    for (let i = 0; i < 16; i++) M[i] = view.getUint32(off + i * 4, true)
    let A = a0
    let B = b0
    let C = c0
    let D = d0
    for (let i = 0; i < 64; i++) {
      let F: number
      let g: number
      if (i < 16) {
        F = (B & C) | (~B & D)
        g = i
      } else if (i < 32) {
        F = (D & B) | (~D & C)
        g = (5 * i + 1) % 16
      } else if (i < 48) {
        F = B ^ C ^ D
        g = (3 * i + 5) % 16
      } else {
        F = C ^ (B | ~D)
        g = (7 * i) % 16
      }
      F = (F + A + K[i]! + M[g]!) >>> 0
      A = D
      D = C
      C = B
      const s = S[i]!
      B = (B + ((F << s) | (F >>> (32 - s)))) >>> 0
    }
    a0 = (a0 + A) >>> 0
    b0 = (b0 + B) >>> 0
    c0 = (c0 + C) >>> 0
    d0 = (d0 + D) >>> 0
  }

  const out = new Uint8Array(16)
  const ov = new DataView(out.buffer)
  ov.setUint32(0, a0, true)
  ov.setUint32(4, b0, true)
  ov.setUint32(8, c0, true)
  ov.setUint32(12, d0, true)
  return out
}

export function md5Hex(input: BytesInput): string {
  return bytesToHex(md5(input))
}

/** Value for the Content-MD5 header (base64 of the binary digest). */
export function md5Base64(input: BytesInput): string {
  return bytesToBase64(md5(input))
}

// ---------------------------------------------------------------------------
// SHA-256 (WebCrypto when available, JavaScript on HTTP LAN origins)
// ---------------------------------------------------------------------------

/** Lower-case hex SHA-256 of input (strings are UTF-8 encoded). */
export async function sha256Hex(input: BytesInput): Promise<string> {
  const bytes = toBytes(input)
  const subtle = globalThis.crypto?.subtle
  if (!subtle) return bytesToHex(sha256(bytes))
  return bytesToHex(new Uint8Array(await subtle.digest('SHA-256', bytes as Uint8Array<ArrayBuffer>)))
}

// ---------------------------------------------------------------------------
// CRC32 (IEEE 802.3, as used by x-amz-checksum-crc32)
// ---------------------------------------------------------------------------

const CRC32_TABLE = (() => {
  const table = new Uint32Array(256)
  for (let n = 0; n < 256; n++) {
    let c = n
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1
    table[n] = c >>> 0
  }
  return table
})()

/** Incremental CRC32: pass the previous value to continue a running checksum. */
export function crc32(input: BytesInput, previous = 0): number {
  const bytes = toBytes(input)
  let crc = (previous ^ 0xffffffff) >>> 0
  for (const b of bytes) crc = (CRC32_TABLE[(crc ^ b) & 0xff]! ^ (crc >>> 8)) >>> 0
  return (crc ^ 0xffffffff) >>> 0
}

/** Value for the x-amz-checksum-crc32 header (base64 of big-endian uint32). */
export function crc32Base64(input: BytesInput): string {
  const v = crc32(input)
  const b = new Uint8Array(4)
  new DataView(b.buffer).setUint32(0, v, false)
  return bytesToBase64(b)
}
