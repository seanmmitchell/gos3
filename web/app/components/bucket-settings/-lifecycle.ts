// Lifecycle rule editor model <-> S3 LifecycleRule mapping.
//
// The editor works on a flat form (prefix, tags, sizes, expiration, …);
// `formToRule` builds the S3 filter shape (Prefix / Tag / And / size)
// exactly as S3 requires it, and `ruleToForm` reads every filter variant
// (including legacy top-level <Prefix>). Transitions (storage classes) are
// not editable here but preserved round-trip.
//
// File name starts with "-" so Nuxt does not register it as a component.
import type { LifecycleFilter, LifecycleRule, Tag } from '~/types/s3'
import type { KeyValuePair } from '~/types/ui'

export const MAX_LIFECYCLE_RULES = 1000
export const MAX_RULE_ID_LENGTH = 255

export type ExpirationMode = 'none' | 'days' | 'date'

export interface LifecycleRuleForm {
  id: string
  enabled: boolean
  /** Filter: key prefix ('' = whole bucket). */
  prefix: string
  /** Filter: object tags (all must match). */
  tags: KeyValuePair[]
  /** Filter: object size bounds in bytes (null = unset). */
  sizeGreaterThan: number | null
  sizeLessThan: number | null
  expirationMode: ExpirationMode
  expirationDays: number | null
  /** YYYY-MM-DD (midnight UTC). */
  expirationDate: string
  /** Remove delete markers without noncurrent versions (only without days/date). */
  expiredObjectDeleteMarker: boolean
  /** Noncurrent version expiration. */
  noncurrentDays: number | null
  newerNoncurrentVersions: number | null
  /** Abort incomplete multipart uploads after N days. */
  abortDays: number | null
  /** Preserved, not edited. */
  transitions?: LifecycleRule['transitions']
  noncurrentVersionTransitions?: LifecycleRule['noncurrentVersionTransitions']
}

export type LifecycleFormErrors = Partial<Record<
  'id' | 'tags' | 'size' | 'expirationDays' | 'expirationDate' | 'expiredObjectDeleteMarker' | 'noncurrentDays' | 'newerNoncurrentVersions' | 'abortDays' | 'actions',
  string
>>

export function newRuleForm(existingIds: readonly string[] = []): LifecycleRuleForm {
  let n = existingIds.length + 1
  while (existingIds.includes(`rule-${n}`)) n++
  return {
    id: `rule-${n}`,
    enabled: true,
    prefix: '',
    tags: [],
    sizeGreaterThan: null,
    sizeLessThan: null,
    expirationMode: 'days',
    expirationDays: 30,
    expirationDate: '',
    expiredObjectDeleteMarker: false,
    noncurrentDays: null,
    newerNoncurrentVersions: null,
    abortDays: 7
  }
}

const toPair = (t: Tag): KeyValuePair => ({ key: t.key, value: t.value })
const toTag = (p: KeyValuePair): Tag => ({ key: p.key.trim(), value: p.value })

/** "2027-01-01T00:00:00.000Z" → "2027-01-01". */
export function isoToDateInput(iso: string | undefined): string {
  if (!iso) return ''
  const m = /^(\d{4}-\d{2}-\d{2})/.exec(iso)
  return m ? m[1]! : ''
}

/** "2027-01-01" → "2027-01-01T00:00:00.000Z" (S3 requires midnight UTC). */
export function dateInputToIso(d: string): string {
  return `${d}T00:00:00.000Z`
}

/** S3 rule → editor form. */
export function ruleToForm(rule: LifecycleRule): LifecycleRuleForm {
  const f = rule.filter
  let prefix = rule.prefix ?? ''
  let tags: KeyValuePair[] = []
  let gt: number | null = null
  let lt: number | null = null
  if (f) {
    if (f.and) {
      prefix = f.and.prefix ?? ''
      tags = f.and.tags.map(toPair)
      gt = f.and.objectSizeGreaterThan ?? null
      lt = f.and.objectSizeLessThan ?? null
    } else {
      prefix = f.prefix ?? ''
      if (f.tag) tags = [toPair(f.tag)]
      gt = f.objectSizeGreaterThan ?? null
      lt = f.objectSizeLessThan ?? null
    }
  }
  const exp = rule.expiration
  const mode: ExpirationMode = exp?.days !== undefined ? 'days' : exp?.date ? 'date' : 'none'
  return {
    id: rule.id ?? '',
    enabled: rule.status === 'Enabled',
    prefix,
    tags,
    sizeGreaterThan: gt,
    sizeLessThan: lt,
    expirationMode: mode,
    expirationDays: exp?.days ?? null,
    expirationDate: isoToDateInput(exp?.date),
    expiredObjectDeleteMarker: mode === 'none' && !!exp?.expiredObjectDeleteMarker,
    noncurrentDays: rule.noncurrentVersionExpiration?.noncurrentDays ?? null,
    newerNoncurrentVersions: rule.noncurrentVersionExpiration?.newerNoncurrentVersions ?? null,
    abortDays: rule.abortIncompleteMultipartUpload?.daysAfterInitiation ?? null,
    transitions: rule.transitions,
    noncurrentVersionTransitions: rule.noncurrentVersionTransitions
  }
}

const isSet = (n: number | null | undefined): n is number => n !== null && n !== undefined && Number.isFinite(n)

/** Builds the S3 filter from the form's conditions. */
export function buildFilter(form: Pick<LifecycleRuleForm, 'prefix' | 'tags' | 'sizeGreaterThan' | 'sizeLessThan'>): LifecycleFilter {
  const prefix = form.prefix
  const tags = form.tags.filter(t => t.key.trim()).map(toTag)
  const gt = isSet(form.sizeGreaterThan) ? form.sizeGreaterThan : undefined
  const lt = isSet(form.sizeLessThan) ? form.sizeLessThan : undefined
  const conditions = (prefix ? 1 : 0) + tags.length + (gt !== undefined ? 1 : 0) + (lt !== undefined ? 1 : 0)
  if (conditions > 1) {
    return {
      and: {
        prefix: prefix || undefined,
        tags,
        objectSizeGreaterThan: gt,
        objectSizeLessThan: lt
      }
    }
  }
  if (tags.length === 1) return { tag: tags[0] }
  if (gt !== undefined) return { objectSizeGreaterThan: gt }
  if (lt !== undefined) return { objectSizeLessThan: lt }
  return { prefix }
}

/** Editor form → S3 rule (call validateRuleForm first). */
export function formToRule(form: LifecycleRuleForm): LifecycleRule {
  const rule: LifecycleRule = {
    id: form.id.trim() || undefined,
    status: form.enabled ? 'Enabled' : 'Disabled',
    filter: buildFilter(form)
  }
  if (form.expirationMode === 'days' && isSet(form.expirationDays)) {
    rule.expiration = { days: form.expirationDays }
  } else if (form.expirationMode === 'date' && form.expirationDate) {
    rule.expiration = { date: dateInputToIso(form.expirationDate) }
  } else if (form.expiredObjectDeleteMarker) {
    rule.expiration = { expiredObjectDeleteMarker: true }
  }
  if (isSet(form.noncurrentDays)) {
    rule.noncurrentVersionExpiration = {
      noncurrentDays: form.noncurrentDays,
      newerNoncurrentVersions: isSet(form.newerNoncurrentVersions) ? form.newerNoncurrentVersions : undefined
    }
  }
  if (isSet(form.abortDays)) rule.abortIncompleteMultipartUpload = { daysAfterInitiation: form.abortDays }
  if (form.transitions?.length) rule.transitions = form.transitions
  if (form.noncurrentVersionTransitions?.length) rule.noncurrentVersionTransitions = form.noncurrentVersionTransitions
  return rule
}

const positiveInt = (n: number | null) => isSet(n) && Number.isInteger(n) && n > 0

/** Validates a rule form against S3's lifecycle rules. */
export function validateRuleForm(form: LifecycleRuleForm, otherIds: readonly string[] = []): LifecycleFormErrors {
  const e: LifecycleFormErrors = {}
  const id = form.id.trim()
  if (id.length > MAX_RULE_ID_LENGTH) e.id = `At most ${MAX_RULE_ID_LENGTH} characters`
  else if (id && otherIds.includes(id)) e.id = 'Another rule already uses this ID'

  const keys = form.tags.map(t => t.key.trim())
  if (keys.some(k => !k)) e.tags = 'Tag keys are required'
  else if (new Set(keys).size !== keys.length) e.tags = 'Tag keys must be unique'

  const gt = form.sizeGreaterThan
  const lt = form.sizeLessThan
  if ((isSet(gt) && (gt < 0 || !Number.isInteger(gt))) || (isSet(lt) && (lt <= 0 || !Number.isInteger(lt)))) {
    e.size = 'Sizes must be whole numbers of bytes'
  } else if (isSet(gt) && isSet(lt) && gt >= lt) {
    e.size = '"Larger than" must be smaller than "smaller than"'
  }

  if (form.expirationMode === 'days' && !positiveInt(form.expirationDays)) e.expirationDays = 'Must be a whole number ≥ 1'
  if (form.expirationMode === 'date') {
    if (!/^\d{4}-\d{2}-\d{2}$/.test(form.expirationDate) || Number.isNaN(Date.parse(dateInputToIso(form.expirationDate)))) {
      e.expirationDate = 'Pick a date'
    }
  }
  if (form.expiredObjectDeleteMarker) {
    if (form.expirationMode !== 'none') e.expiredObjectDeleteMarker = 'Cannot be combined with an expiration in days or by date'
    else if (keys.length) e.expiredObjectDeleteMarker = 'Cannot be combined with tag filters'
  }
  if (isSet(form.noncurrentDays) && !positiveInt(form.noncurrentDays)) e.noncurrentDays = 'Must be a whole number ≥ 1'
  if (isSet(form.newerNoncurrentVersions)) {
    if (!positiveInt(form.newerNoncurrentVersions) || form.newerNoncurrentVersions > 100) e.newerNoncurrentVersions = 'Must be between 1 and 100'
    else if (!isSet(form.noncurrentDays)) e.newerNoncurrentVersions = 'Requires "noncurrent days"'
  }
  if (isSet(form.abortDays)) {
    if (!positiveInt(form.abortDays)) e.abortDays = 'Must be a whole number ≥ 1'
    else if (keys.length) e.abortDays = 'Cannot be combined with tag filters'
  }

  const hasAction = (form.expirationMode !== 'none') || form.expiredObjectDeleteMarker || isSet(form.noncurrentDays)
    || isSet(form.abortDays) || !!form.transitions?.length || !!form.noncurrentVersionTransitions?.length
  if (!hasAction) e.actions = 'Choose at least one action'
  return e
}

/** Human readable filter summary. */
export function describeFilter(rule: LifecycleRule): string {
  const f = ruleToForm(rule)
  const parts: string[] = []
  if (f.prefix) parts.push(`prefix "${f.prefix}"`)
  for (const t of f.tags) parts.push(`tag ${t.key}=${t.value}`)
  if (isSet(f.sizeGreaterThan)) parts.push(`> ${f.sizeGreaterThan} B`)
  if (isSet(f.sizeLessThan)) parts.push(`< ${f.sizeLessThan} B`)
  return parts.length ? parts.join(', ') : 'Whole bucket'
}

/** Human readable action summaries. */
export function describeActions(rule: LifecycleRule): string[] {
  const out: string[] = []
  const e = rule.expiration
  if (e?.days !== undefined) out.push(`Expire after ${e.days} day${e.days === 1 ? '' : 's'}`)
  else if (e?.date) out.push(`Expire on ${isoToDateInput(e.date)}`)
  if (e?.expiredObjectDeleteMarker) out.push('Remove expired delete markers')
  const nv = rule.noncurrentVersionExpiration
  if (nv?.noncurrentDays !== undefined) {
    out.push(`Delete noncurrent versions after ${nv.noncurrentDays} day${nv.noncurrentDays === 1 ? '' : 's'}${nv.newerNoncurrentVersions ? ` (keep ${nv.newerNoncurrentVersions} newest)` : ''}`)
  }
  const a = rule.abortIncompleteMultipartUpload
  if (a) out.push(`Abort incomplete uploads after ${a.daysAfterInitiation} day${a.daysAfterInitiation === 1 ? '' : 's'}`)
  if (rule.transitions?.length) out.push(`${rule.transitions.length} transition(s)`)
  if (rule.noncurrentVersionTransitions?.length) out.push(`${rule.noncurrentVersionTransitions.length} noncurrent transition(s)`)
  return out
}
