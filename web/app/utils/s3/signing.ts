// aws4fetch supplies SigV4 canonicalisation. Its built-in SHA-256/HMAC use
// WebCrypto, which browsers omit on HTTP LAN origins. Keep that native path
// on secure origins and use noble's JavaScript primitives otherwise, without
// installing a global crypto polyfill or changing request canonicalisation.
import { hmac } from '@noble/hashes/hmac.js'
import { sha256 } from '@noble/hashes/sha2.js'
import { AwsClient as NativeAwsClient, AwsV4Signer } from 'aws4fetch'
import { bytesToHex, sha256Hex, toBytes } from './hash'

class HttpSigner extends AwsV4Signer {
  override async signature(): Promise<string> {
    const date = this.datetime.slice(0, 8)
    const cacheKey = [this.secretAccessKey, date, this.region, this.service].join()
    let key = this.cache.get(cacheKey)
    if (!key) {
      let derived = hmac(sha256, toBytes(`AWS4${this.secretAccessKey}`), toBytes(date))
      for (const part of [this.region, this.service, 'aws4_request']) {
        derived = hmac(sha256, derived, toBytes(part))
      }
      key = Uint8Array.from(derived).buffer
      this.cache.set(cacheKey, key)
    }
    const stringToSign = [
      'AWS4-HMAC-SHA256', this.datetime, this.credentialString,
      await sha256Hex(await this.canonicalString())
    ].join('\n')
    return bytesToHex(hmac(sha256, new Uint8Array(key), toBytes(stringToSign)))
  }

  override async hexBodyHash(): Promise<string> {
    const header = this.headers.get('X-Amz-Content-Sha256')
    if (header) return header
    if (this.service === 's3' && this.signQuery) return 'UNSIGNED-PAYLOAD'
    if (this.body && typeof this.body !== 'string' && !(this.body instanceof ArrayBuffer) && !ArrayBuffer.isView(this.body)) {
      throw new TypeError('Signing this body requires an X-Amz-Content-Sha256 header')
    }
    return sha256Hex(this.body || '')
  }
}

/** URL-based request signing shared by the S3 and management clients. */
export class AwsClient {
  private readonly native: NativeAwsClient

  constructor(options: ConstructorParameters<typeof NativeAwsClient>[0]) {
    this.native = new NativeAwsClient(options)
  }

  async sign(url: string, init: NonNullable<Parameters<NativeAwsClient['sign']>[1]> = {}): Promise<Request> {
    if (globalThis.crypto?.subtle) return this.native.sign(url, init)
    const signer = new HttpSigner({ ...init, ...this.native, ...init.aws, url })
    const { aws: _aws, ...requestInit } = init
    const signed = await signer.sign()
    return new Request(signed.url.toString(), { ...requestInit, ...signed })
  }
}
