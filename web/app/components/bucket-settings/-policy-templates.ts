// Bucket policy templates (resource policies with a Principal).
//
// File name starts with "-" so Nuxt does not register it as a component.
import type { PolicyDocument, PolicyStatement } from '~/types/admin'
import { POLICY_VERSION, formatPolicy } from '~/components/admin/-policies'

export type BucketPolicyTemplateId = 'public-read' | 'user-readonly' | 'deny-insecure-transport' | 'deny-unencrypted-uploads'

export interface BucketPolicyTemplate {
  id: BucketPolicyTemplateId
  label: string
  description: string
  /** Needs an IAM user name. */
  needsUser: boolean
  build: (bucket: string, opts: { user?: string }) => PolicyStatement[]
}

const arn = (bucket: string, path?: string) => `arn:aws:s3:::${bucket}${path !== undefined ? `/${path}` : ''}`

export const BUCKET_POLICY_TEMPLATES: readonly BucketPolicyTemplate[] = [
  {
    id: 'public-read',
    label: 'Public read',
    description: 'Anyone can download objects (no listing).',
    needsUser: false,
    build: bucket => [
      { Sid: 'PublicRead', Effect: 'Allow', Principal: '*', Action: ['s3:GetObject'], Resource: [arn(bucket, '*')] }
    ]
  },
  {
    id: 'user-readonly',
    label: 'Read-only for a user',
    description: 'Lets one IAM user list and download objects in this bucket.',
    needsUser: true,
    build: (bucket, { user }) => {
      const principal = { AWS: [user || 'USERNAME'] }
      return [
        { Sid: `ReadOnlyList${sidSuffix(user)}`, Effect: 'Allow', Principal: principal, Action: ['s3:ListBucket', 's3:GetBucketLocation'], Resource: [arn(bucket)] },
        { Sid: `ReadOnlyGet${sidSuffix(user)}`, Effect: 'Allow', Principal: principal, Action: ['s3:GetObject', 's3:GetObjectVersion'], Resource: [arn(bucket, '*')] }
      ]
    }
  },
  {
    id: 'deny-insecure-transport',
    label: 'Deny insecure transport',
    description: 'Rejects every request that is not made over HTTPS.',
    needsUser: false,
    build: bucket => [
      {
        Sid: 'DenyInsecureTransport',
        Effect: 'Deny',
        Principal: '*',
        Action: ['s3:*'],
        Resource: [arn(bucket), arn(bucket, '*')],
        Condition: { Bool: { 'aws:SecureTransport': 'false' } }
      }
    ]
  },
  {
    id: 'deny-unencrypted-uploads',
    label: 'Deny unencrypted uploads',
    description: 'Rejects PutObject requests without an x-amz-server-side-encryption header.',
    needsUser: false,
    build: bucket => [
      {
        Sid: 'DenyUnencryptedUploads',
        Effect: 'Deny',
        Principal: '*',
        Action: ['s3:PutObject'],
        Resource: [arn(bucket, '*')],
        Condition: { Null: { 's3:x-amz-server-side-encryption': 'true' } }
      }
    ]
  }
]

function sidSuffix(user: string | undefined): string {
  const s = (user ?? '').replace(/[^A-Za-z0-9]/g, '')
  return s ? `-${s}` : ''
}

/** Full policy document for a template. */
export function bucketPolicyTemplate(id: BucketPolicyTemplateId, bucket: string, opts: { user?: string } = {}): PolicyDocument {
  const t = BUCKET_POLICY_TEMPLATES.find(x => x.id === id)
  if (!t) throw new Error(`Unknown template ${id}`)
  return { Version: POLICY_VERSION, Statement: t.build(bucket, opts) }
}

/**
 * Applies a template to the current editor text: when the current text is a
 * valid policy, the template statements are appended (statements with the
 * same Sid are replaced); otherwise the template replaces the text.
 */
export function applyBucketTemplate(currentText: string, id: BucketPolicyTemplateId, bucket: string, opts: { user?: string } = {}): string {
  const tpl = bucketPolicyTemplate(id, bucket, opts)
  let current: PolicyDocument | undefined
  try {
    const v = JSON.parse(currentText) as unknown
    if (v && typeof v === 'object' && !Array.isArray(v)) {
      const st = (v as { Statement?: unknown }).Statement
      if (Array.isArray(st) || (st && typeof st === 'object')) current = v as PolicyDocument
    }
  } catch {
    current = undefined
  }
  if (!current) return formatPolicy(tpl)
  const existing = Array.isArray(current.Statement) ? current.Statement : [current.Statement as unknown as PolicyStatement]
  const newSids = new Set(tpl.Statement.map(s => s.Sid).filter(Boolean))
  const kept = existing.filter(s => !s.Sid || !newSids.has(s.Sid))
  return formatPolicy({ ...current, Version: current.Version ?? POLICY_VERSION, Statement: [...kept, ...tpl.Statement] })
}
