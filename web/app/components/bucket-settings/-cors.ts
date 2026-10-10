// Bucket CORS rule editor model.
//
// File name starts with "-" so Nuxt does not register it as a component.
import type { CORSRule } from '~/types/s3'
import { normalizeList } from '~/components/admin/-settings-form'

export const CORS_METHODS = ['GET', 'PUT', 'POST', 'DELETE', 'HEAD'] as const
export const MAX_CORS_RULES = 100

/**
 * ExposeHeader defaults for new rules: what S3 clients in a browser read
 * (the ETag of uploaded parts, versions, content headers). S3 has no
 * wildcard for ExposeHeader: user metadata is only readable cross-origin
 * when each x-amz-meta-<name> is listed.
 */
export const DEFAULT_EXPOSE_HEADERS = [
  'ETag', 'Content-Disposition', 'Content-Encoding', 'Content-Language', 'Content-Range', 'Accept-Ranges',
  'x-amz-request-id', 'x-amz-version-id', 'x-amz-delete-marker'
] as const

export interface CorsRuleForm {
  id: string
  allowedOrigins: string[]
  allowedMethods: string[]
  allowedHeaders: string[]
  exposeHeaders: string[]
  maxAgeSeconds: number | null
}

export type CorsRuleErrors = Partial<Record<'id' | 'allowedOrigins' | 'allowedMethods' | 'allowedHeaders' | 'exposeHeaders' | 'maxAgeSeconds', string>>

export function newCorsRule(): CorsRuleForm {
  return {
    id: '',
    allowedOrigins: ['*'],
    allowedMethods: ['GET', 'HEAD'],
    allowedHeaders: ['*'],
    exposeHeaders: [...DEFAULT_EXPOSE_HEADERS],
    maxAgeSeconds: 3600
  }
}

export function corsRuleToForm(r: CORSRule): CorsRuleForm {
  return {
    id: r.id ?? '',
    allowedOrigins: [...r.allowedOrigins],
    allowedMethods: [...r.allowedMethods],
    allowedHeaders: [...r.allowedHeaders],
    exposeHeaders: [...r.exposeHeaders],
    maxAgeSeconds: r.maxAgeSeconds ?? null
  }
}

export function formToCorsRule(f: CorsRuleForm): CORSRule {
  const methods = CORS_METHODS.filter(m => f.allowedMethods.includes(m))
  return {
    id: f.id.trim() || undefined,
    allowedOrigins: normalizeList(f.allowedOrigins),
    allowedMethods: methods,
    allowedHeaders: normalizeList(f.allowedHeaders),
    exposeHeaders: normalizeList(f.exposeHeaders),
    maxAgeSeconds: f.maxAgeSeconds !== null && Number.isFinite(f.maxAgeSeconds) ? Math.round(f.maxAgeSeconds) : undefined
  }
}

const oneWildcard = (s: string) => s.split('*').length <= 2

export function validateCorsRule(f: CorsRuleForm): CorsRuleErrors {
  const e: CorsRuleErrors = {}
  if (f.id.length > 255) e.id = 'At most 255 characters'
  const origins = normalizeList(f.allowedOrigins)
  if (!origins.length) e.allowedOrigins = 'At least one origin is required'
  else {
    const bad = origins.find(o => !oneWildcard(o) || /\s/.test(o))
    if (bad) e.allowedOrigins = `"${bad}" may contain at most one "*"`
  }
  if (!f.allowedMethods.some(m => (CORS_METHODS as readonly string[]).includes(m))) e.allowedMethods = 'Select at least one method'
  const badHeader = normalizeList(f.allowedHeaders).find(h => !oneWildcard(h) || !/^[\w*-]+$/.test(h))
  if (badHeader) e.allowedHeaders = `"${badHeader}" is not a valid header name`
  const badExpose = normalizeList(f.exposeHeaders).find(h => !/^[\w-]+$/.test(h))
  if (badExpose) e.exposeHeaders = `"${badExpose}" is not a valid header name`
  if (f.maxAgeSeconds !== null && (!Number.isInteger(f.maxAgeSeconds) || f.maxAgeSeconds < 0)) e.maxAgeSeconds = 'Must be a whole number ≥ 0'
  return e
}

export function hasErrors(e: object): boolean {
  return Object.values(e).some(Boolean)
}
