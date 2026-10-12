// PublicAccessBlock (?publicAccessBlock) get/put/delete on top of the
// generic S3Client.send(); the shared S3 client has no wrapper for it yet.
//
// File name starts with "-" so Nuxt does not register it as a component.
import type { S3Client } from '~/utils/s3/client'
import { S3Error } from '~/utils/s3/errors'
import { bool, child, parseXml, t, xmlDocument } from '~/utils/s3/xml'

export interface PublicAccessBlockConfig {
  blockPublicAcls: boolean
  ignorePublicAcls: boolean
  blockPublicPolicy: boolean
  restrictPublicBuckets: boolean
}

export const PAB_FIELDS: readonly { key: keyof PublicAccessBlockConfig, xml: string, label: string, description: string }[] = [
  { key: 'blockPublicAcls', xml: 'BlockPublicAcls', label: 'Block public ACLs', description: 'Reject requests that set a public canned ACL on the bucket or its objects.' },
  { key: 'ignorePublicAcls', xml: 'IgnorePublicAcls', label: 'Ignore public ACLs', description: 'Ignore existing public ACLs when authorizing requests.' },
  { key: 'blockPublicPolicy', xml: 'BlockPublicPolicy', label: 'Block public bucket policies', description: 'Reject bucket policies that grant public access.' },
  { key: 'restrictPublicBuckets', xml: 'RestrictPublicBuckets', label: 'Restrict public buckets', description: 'Only authenticated principals may access the bucket even if its policy is public.' }
]

export const PAB_NONE: Readonly<PublicAccessBlockConfig> = Object.freeze({
  blockPublicAcls: false,
  ignorePublicAcls: false,
  blockPublicPolicy: false,
  restrictPublicBuckets: false
})

export function parsePublicAccessBlock(body: string): PublicAccessBlockConfig {
  const root = parseXml(body)
  const el = root.localName === 'PublicAccessBlockConfiguration' ? root : child(root, 'PublicAccessBlockConfiguration') ?? root
  const out = { ...PAB_NONE }
  for (const f of PAB_FIELDS) out[f.key] = bool(el, f.xml) === true
  return out
}

export function buildPublicAccessBlock(cfg: PublicAccessBlockConfig): string {
  return xmlDocument('PublicAccessBlockConfiguration', ...PAB_FIELDS.map(f => t(f.xml, cfg[f.key])))
}

/** null when no configuration exists (404 NoSuchPublicAccessBlockConfiguration). */
export async function getPublicAccessBlock(s3: S3Client, bucket: string): Promise<PublicAccessBlockConfig | null> {
  try {
    const { body } = await s3.send({ method: 'GET', bucket, query: { publicAccessBlock: '' } })
    return parsePublicAccessBlock(body)
  } catch (e) {
    if (e instanceof S3Error && e.status === 404 && e.code !== 'NoSuchBucket') return null
    throw e
  }
}

export async function putPublicAccessBlock(s3: S3Client, bucket: string, cfg: PublicAccessBlockConfig): Promise<void> {
  await s3.send({
    method: 'PUT',
    bucket,
    query: { publicAccessBlock: '' },
    body: buildPublicAccessBlock(cfg),
    md5: true,
    headers: { 'Content-Type': 'application/xml' }
  })
}

export async function deletePublicAccessBlock(s3: S3Client, bucket: string): Promise<void> {
  await s3.send({ method: 'DELETE', bucket, query: { publicAccessBlock: '' } })
}
