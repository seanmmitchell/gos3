// IAM policy helpers: templates for custom identity policies, client-side
// document validation (mirrors the server's checks in internal/iam/parse.go
// so obvious mistakes are flagged before saving) and name validation.
//
// File name starts with "-" so Nuxt does not register it as a component.
import type { PolicyDocument, PolicyStatement } from '~/types/admin'

export const POLICY_VERSION = '2012-10-17'

/** Characters allowed in IAM user and policy names. */
const IAM_NAME_RE = /^[\w+=,.@-]+$/

/** User names: 1–64 of [A-Za-z0-9+=,.@_-], not "root"/"anonymous". */
export function validateUserName(name: string): string | undefined {
  if (!name) return 'Name is required'
  if (name.length > 64) return 'At most 64 characters'
  if (!IAM_NAME_RE.test(name)) return 'Only letters, digits and + = , . @ _ - are allowed'
  if (/^(root|anonymous)$/i.test(name)) return `"${name}" is reserved`
  return undefined
}

/** Policy names: 1–128 of [A-Za-z0-9+=,.@_-]. */
export function validatePolicyName(name: string, builtinNames: readonly string[] = []): string | undefined {
  if (!name) return 'Name is required'
  if (name.length > 128) return 'At most 128 characters'
  if (!IAM_NAME_RE.test(name)) return 'Only letters, digits and + = , . @ _ - are allowed'
  if (builtinNames.includes(name)) return `"${name}" is a built-in policy`
  return undefined
}

function asList(v: unknown): unknown[] {
  if (v === undefined || v === null) return []
  return Array.isArray(v) ? v : [v]
}

const ACTION_RE = /^(\*|(s3|admin):[A-Za-z0-9*?]+)$/i

/**
 * Validates a parsed policy document. `kind` selects identity-policy rules
 * (no Principal, s3:/admin: actions, ARN or "*" resources) or bucket-policy
 * rules (Principal required, s3: actions, resources inside `bucket`).
 * Returns the first problem or undefined.
 */
export function validatePolicyDocument(value: unknown, kind: 'identity' | 'bucket', bucket?: string): string | undefined {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return 'The policy must be a JSON object'
  const doc = value as Record<string, unknown>
  if (doc.Version !== undefined && doc.Version !== POLICY_VERSION && doc.Version !== '2008-10-17') {
    return `Version must be "${POLICY_VERSION}"`
  }
  const statements = asList(doc.Statement)
  if (!statements.length) return 'Missing required field Statement'
  const sids = new Set<string>()
  for (const [i, raw] of statements.entries()) {
    const where = `Statement ${i + 1}`
    if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) return `${where} must be an object`
    const st = raw as Record<string, unknown>
    if (typeof st.Sid === 'string' && st.Sid) {
      if (sids.has(st.Sid)) return `${where}: duplicate Sid "${st.Sid}"`
      sids.add(st.Sid)
    }
    if (st.Effect !== 'Allow' && st.Effect !== 'Deny') return `${where}: Effect must be "Allow" or "Deny"`
    const hasAction = st.Action !== undefined
    const hasNotAction = st.NotAction !== undefined
    if (hasAction === hasNotAction) return `${where}: exactly one of Action or NotAction is required`
    if (st.Resource !== undefined && st.NotResource !== undefined) return `${where}: use Resource or NotResource, not both`
    if (st.Resource === undefined && st.NotResource === undefined) return `${where}: missing required field Resource`
    for (const a of asList(hasAction ? st.Action : st.NotAction)) {
      if (typeof a !== 'string' || !ACTION_RE.test(a)) return `${where}: invalid action ${JSON.stringify(a)}`
      if (kind === 'bucket' && /^admin:/i.test(a)) return `${where}: admin actions are not allowed in bucket policies`
    }
    for (const r of asList(st.Resource ?? st.NotResource)) {
      if (typeof r !== 'string') return `${where}: invalid resource ${JSON.stringify(r)}`
      if (kind === 'identity') {
        if (r !== '*' && !/^arn:[^:]+:[^:]+:[^:]*:[^:]*:.+$/.test(r)) return `${where}: invalid resource "${r}" (use "*" or an ARN)`
      } else {
        if (!r.startsWith('arn:aws:s3:::')) return `${where}: resources must start with arn:aws:s3:::`
        const b = r.slice('arn:aws:s3:::'.length).split('/')[0] ?? ''
        if (!b) return `${where}: resource "${r}" has no bucket`
        if (bucket && !globMatch(b, bucket)) return `${where}: resource "${r}" does not belong to bucket ${bucket}`
      }
    }
    const hasPrincipal = st.Principal !== undefined || st.NotPrincipal !== undefined
    if (kind === 'identity' && hasPrincipal) return `${where}: identity policies cannot contain Principal`
    if (kind === 'bucket' && !hasPrincipal) return `${where}: missing required field Principal`
    if (st.Condition !== undefined && (typeof st.Condition !== 'object' || st.Condition === null || Array.isArray(st.Condition))) {
      return `${where}: Condition must be an object`
    }
  }
  return undefined
}

/** Minimal * / ? glob match (bucket patterns in resource ARNs). */
export function globMatch(pattern: string, s: string): boolean {
  const re = new RegExp(`^${pattern.replace(/[.+^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '.*').replace(/\?/g, '.')}$`)
  return re.test(s)
}

/** Parses policy JSON text; returns the document or an error message. */
export function parsePolicyText(text: string, kind: 'identity' | 'bucket', bucket?: string): { doc?: PolicyDocument, error?: string } {
  let v: unknown
  try {
    v = JSON.parse(text)
  } catch (e) {
    return { error: e instanceof Error ? e.message : 'Invalid JSON' }
  }
  const error = validatePolicyDocument(v, kind, bucket)
  return error ? { error } : { doc: v as PolicyDocument }
}

export function formatPolicy(doc: PolicyDocument | unknown): string {
  return JSON.stringify(doc, null, 2)
}

// ---------------------------------------------------------------------------
// Identity policy templates
// ---------------------------------------------------------------------------

export type TemplateParam = 'bucket' | 'cidr'

export interface PolicyTemplateParams {
  bucket?: string
  cidr?: string
}

export interface IamPolicyTemplate {
  id: 'readonly-bucket' | 'readwrite-bucket' | 'prefix-per-user' | 'deny-delete' | 'ip-restricted'
  label: string
  description: string
  /** Parameters the template needs (shown as inputs). */
  params: TemplateParam[]
  /** Parameters that must be non-empty. */
  required: TemplateParam[]
  build: (p: PolicyTemplateParams) => PolicyDocument
}

const s3Arn = (bucket: string, path?: string) => `arn:aws:s3:::${bucket}${path !== undefined ? `/${path}` : ''}`

/** Default when a template is applied without a bucket: all buckets. */
const bucketOrAll = (b?: string) => (b?.trim() ? b.trim() : '*')

function doc(...statements: PolicyStatement[]): PolicyDocument {
  return { Version: POLICY_VERSION, Statement: statements }
}

export const IAM_POLICY_TEMPLATES: readonly IamPolicyTemplate[] = [
  {
    id: 'readonly-bucket',
    label: 'Read-only bucket',
    description: 'List and download objects in one bucket.',
    params: ['bucket'],
    required: ['bucket'],
    build: ({ bucket }) => {
      const b = bucketOrAll(bucket)
      return doc(
        { Sid: 'ListBucket', Effect: 'Allow', Action: ['s3:ListBucket', 's3:ListBucketVersions', 's3:GetBucketLocation'], Resource: [s3Arn(b)] },
        { Sid: 'ReadObjects', Effect: 'Allow', Action: ['s3:GetObject', 's3:GetObjectVersion', 's3:GetObjectTagging', 's3:GetObjectAttributes'], Resource: [s3Arn(b, '*')] }
      )
    }
  },
  {
    id: 'readwrite-bucket',
    label: 'Read-write bucket',
    description: 'Full object access (list, read, write, delete, multipart) in one bucket.',
    params: ['bucket'],
    required: ['bucket'],
    build: ({ bucket }) => {
      const b = bucketOrAll(bucket)
      return doc(
        {
          Sid: 'ListBucket',
          Effect: 'Allow',
          Action: ['s3:ListBucket', 's3:ListBucketVersions', 's3:ListBucketMultipartUploads', 's3:GetBucketLocation'],
          Resource: [s3Arn(b)]
        },
        {
          Sid: 'ReadWriteObjects',
          Effect: 'Allow',
          Action: ['s3:GetObject*', 's3:PutObject*', 's3:DeleteObject*', 's3:AbortMultipartUpload', 's3:ListMultipartUploadParts'],
          Resource: [s3Arn(b, '*')]
        }
      )
    }
  },
  {
    id: 'prefix-per-user',
    label: 'Home folder per user',
    description: 'Each user may only use objects under <bucket>/<their user name>/ (${aws:username}).',
    params: ['bucket'],
    required: ['bucket'],
    build: ({ bucket }) => {
      const b = bucketOrAll(bucket)
      return doc(
        { Sid: 'BucketLocation', Effect: 'Allow', Action: ['s3:GetBucketLocation'], Resource: [s3Arn(b)] },
        {
          Sid: 'ListOwnPrefix',
          Effect: 'Allow',
          Action: ['s3:ListBucket'],
          Resource: [s3Arn(b)],
          Condition: { StringLike: { 's3:prefix': ['', '${aws:username}', '${aws:username}/*'] } }
        },
        {
          Sid: 'OwnObjects',
          Effect: 'Allow',
          Action: ['s3:GetObject*', 's3:PutObject*', 's3:DeleteObject*', 's3:AbortMultipartUpload', 's3:ListMultipartUploadParts'],
          Resource: [s3Arn(b, '${aws:username}/*')]
        }
      )
    }
  },
  {
    id: 'deny-delete',
    label: 'Deny deletes',
    description: 'Explicitly deny deleting objects, versions and buckets (combine with an allow policy). Empty bucket = all buckets.',
    params: ['bucket'],
    required: [],
    build: ({ bucket }) => {
      const b = bucketOrAll(bucket)
      return doc({
        Sid: 'DenyDelete',
        Effect: 'Deny',
        Action: ['s3:DeleteObject', 's3:DeleteObjectVersion', 's3:DeleteBucket'],
        Resource: [s3Arn(b), s3Arn(b, '*')]
      })
    }
  },
  {
    id: 'ip-restricted',
    label: 'IP restricted',
    description: 'Deny every request that does not come from the given network (combine with an allow policy).',
    params: ['cidr'],
    required: ['cidr'],
    build: ({ cidr }) =>
      doc({
        Sid: 'DenyOutsideNetwork',
        Effect: 'Deny',
        Action: '*',
        Resource: '*',
        Condition: { NotIpAddress: { 'aws:SourceIp': (cidr ?? '').split(/[\s,]+/).filter(Boolean) } }
      })
  }
]

/** Validates "10.0.0.0/8" / "192.168.1.10" / IPv6 CIDR lists (comma separated). */
export function validateCidrList(v: string): string | undefined {
  const items = v.split(/[\s,]+/).filter(Boolean)
  if (!items.length) return 'At least one network is required'
  for (const it of items) {
    const [ip, bits, ...rest] = it.split('/')
    if (rest.length || !ip) return `"${it}" is not a valid CIDR`
    const v4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(ip)
    const isV6 = !v4 && /^[0-9a-f:]+$/i.test(ip) && ip.includes(':')
    if (v4 && v4.slice(1).some(o => Number(o) > 255)) return `"${it}" is not a valid IPv4 address`
    if (!v4 && !isV6) return `"${it}" is not a valid IP address`
    if (bits !== undefined) {
      const n = Number(bits)
      if (!/^\d+$/.test(bits) || n > (v4 ? 32 : 128)) return `"${it}" has an invalid prefix length`
    }
  }
  return undefined
}

/** Missing required parameters / invalid values for a template. */
export function templateParamErrors(t: IamPolicyTemplate, p: PolicyTemplateParams): Partial<Record<TemplateParam, string>> {
  const out: Partial<Record<TemplateParam, string>> = {}
  for (const k of t.required) if (!p[k]?.trim()) out[k] = 'Required'
  if (t.params.includes('cidr') && p.cidr?.trim()) {
    const e = validateCidrList(p.cidr)
    if (e) out.cidr = e
  }
  if (t.params.includes('bucket') && p.bucket?.trim() && !/^[a-z0-9.*?-]+$/.test(p.bucket.trim())) {
    out.bucket = 'Invalid bucket name or pattern'
  }
  return out
}

/** Renders a template as pretty JSON. */
export function renderIamTemplate(id: IamPolicyTemplate['id'], p: PolicyTemplateParams): string {
  const t = IAM_POLICY_TEMPLATES.find(x => x.id === id)
  if (!t) throw new Error(`Unknown template ${id}`)
  return formatPolicy(t.build(p))
}

/** Empty starting document for a new custom policy. */
export function emptyIdentityPolicy(): string {
  return formatPolicy(doc({ Effect: 'Allow', Action: ['s3:ListAllMyBuckets'], Resource: ['*'] }))
}

// ---------------------------------------------------------------------------
// Simulator
// ---------------------------------------------------------------------------

/** Common actions offered by the authorization simulator (free text allowed too). */
export const SIMULATOR_ACTIONS: readonly string[] = [
  's3:ListAllMyBuckets',
  's3:CreateBucket',
  's3:DeleteBucket',
  's3:ListBucket',
  's3:ListBucketVersions',
  's3:ListBucketMultipartUploads',
  's3:GetBucketLocation',
  's3:GetObject',
  's3:GetObjectVersion',
  's3:PutObject',
  's3:DeleteObject',
  's3:DeleteObjectVersion',
  's3:GetObjectTagging',
  's3:PutObjectTagging',
  's3:GetObjectRetention',
  's3:PutObjectRetention',
  's3:GetObjectLegalHold',
  's3:PutObjectLegalHold',
  's3:BypassGovernanceRetention',
  's3:AbortMultipartUpload',
  's3:GetBucketPolicy',
  's3:PutBucketPolicy',
  's3:DeleteBucketPolicy',
  's3:GetBucketVersioning',
  's3:PutBucketVersioning',
  's3:GetBucketCORS',
  's3:PutBucketCORS',
  's3:GetLifecycleConfiguration',
  's3:PutLifecycleConfiguration',
  's3:GetBucketTagging',
  's3:PutBucketTagging',
  's3:GetEncryptionConfiguration',
  's3:PutEncryptionConfiguration',
  's3:GetBucketObjectLockConfiguration',
  's3:PutBucketObjectLockConfiguration',
  's3:GetBucketAcl',
  's3:PutBucketAcl',
  'admin:ServerInfo',
  'admin:GetStats',
  'admin:GetConfig',
  'admin:UpdateConfig',
  'admin:ListUsers',
  'admin:CreateUser',
  'admin:ListPolicies',
  'admin:PutPolicy',
  'admin:ForceDeleteBucket',
  'admin:GetDiscovery',
  'admin:ScanNetwork'
]

/**
 * Parses simulator conditions from "key=value[,value]" lines into the
 * request's `conditions` map, e.g. "aws:SourceIp=10.0.0.1".
 */
export function parseConditionLines(text: string): { conditions: Record<string, string[]>, error?: string } {
  const conditions: Record<string, string[]> = {}
  for (const [i, raw] of text.split('\n').entries()) {
    const line = raw.trim()
    if (!line) continue
    const eq = line.indexOf('=')
    if (eq <= 0) return { conditions, error: `Line ${i + 1}: use key=value` }
    const key = line.slice(0, eq).trim()
    const values = line.slice(eq + 1).split(',').map(s => s.trim()).filter(Boolean)
    conditions[key] = [...(conditions[key] ?? []), ...values]
  }
  return { conditions }
}
