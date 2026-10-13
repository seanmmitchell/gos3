import { describe, expect, it } from 'vitest'
import type { LifecycleRule } from '~/types/s3'
import {
  buildFilter,
  dateInputToIso,
  describeActions,
  describeFilter,
  formToRule,
  isoToDateInput,
  newRuleForm,
  ruleToForm,
  validateRuleForm
} from '~/components/bucket-settings/-lifecycle'
import { corsRuleToForm, formToCorsRule, hasErrors, newCorsRule, validateCorsRule } from '~/components/bucket-settings/-cors'
import { buildPublicAccessBlock, parsePublicAccessBlock } from '~/components/bucket-settings/-public-access-block'
import { COMPRESSION_MODE_TEXT, compressionConflict, compressionModeText } from '~/components/bucket-settings/-compression'
import { COMPRESSION_MODES } from '~/components/admin/-settings-form'
import { buildLifecycle } from '~/utils/s3/builders'
import { parseLifecycle } from '~/utils/s3/parsers'

const NO_FILTER = { prefix: '', tags: [], sizeGreaterThan: null, sizeLessThan: null }

describe('lifecycle filter mapping', () => {
  it('uses an empty prefix for the whole bucket', () => {
    expect(buildFilter(NO_FILTER)).toEqual({ prefix: '' })
  })

  it('uses a bare condition when there is exactly one', () => {
    expect(buildFilter({ ...NO_FILTER, prefix: 'logs/' })).toEqual({ prefix: 'logs/' })
    expect(buildFilter({ ...NO_FILTER, tags: [{ key: 'tmp', value: '1' }] })).toEqual({ tag: { key: 'tmp', value: '1' } })
    expect(buildFilter({ ...NO_FILTER, sizeGreaterThan: 1024 })).toEqual({ objectSizeGreaterThan: 1024 })
    expect(buildFilter({ ...NO_FILTER, sizeLessThan: 10 })).toEqual({ objectSizeLessThan: 10 })
  })

  it('uses And when combining conditions', () => {
    expect(buildFilter({ prefix: 'a/', tags: [{ key: 'k', value: 'v' }], sizeGreaterThan: null, sizeLessThan: 5 })).toEqual({
      and: { prefix: 'a/', tags: [{ key: 'k', value: 'v' }], objectSizeGreaterThan: undefined, objectSizeLessThan: 5 }
    })
    expect(buildFilter({ ...NO_FILTER, tags: [{ key: 'a', value: '1' }, { key: 'b', value: '2' }] }).and?.prefix).toBeUndefined()
  })
})

describe('lifecycle rule <-> form', () => {
  it('maps a full rule to the form and back', () => {
    const rule: LifecycleRule = {
      id: 'r1',
      status: 'Enabled',
      filter: { and: { prefix: 'logs/', tags: [{ key: 'env', value: 'dev' }], objectSizeGreaterThan: 100 } },
      expiration: { days: 30 },
      noncurrentVersionExpiration: { noncurrentDays: 7, newerNoncurrentVersions: 3 },
      abortIncompleteMultipartUpload: { daysAfterInitiation: 2 },
      transitions: [{ days: 10, storageClass: 'GLACIER' }]
    }
    const f = ruleToForm(rule)
    expect(f).toMatchObject({
      id: 'r1', enabled: true, prefix: 'logs/', tags: [{ key: 'env', value: 'dev' }], sizeGreaterThan: 100, sizeLessThan: null,
      expirationMode: 'days', expirationDays: 30, noncurrentDays: 7, newerNoncurrentVersions: 3, abortDays: 2
    })
    expect(formToRule(f)).toEqual({ ...rule, filter: { and: { prefix: 'logs/', tags: [{ key: 'env', value: 'dev' }], objectSizeGreaterThan: 100, objectSizeLessThan: undefined } } })
  })

  it('reads legacy top-level prefixes and single-tag filters', () => {
    expect(ruleToForm({ status: 'Disabled', prefix: 'old/', expiration: { days: 1 } })).toMatchObject({ prefix: 'old/', enabled: false })
    expect(ruleToForm({ status: 'Enabled', filter: { tag: { key: 'a', value: 'b' } }, expiration: { days: 1 } }).tags).toEqual([{ key: 'a', value: 'b' }])
    // Converted to a Filter on save.
    const back = formToRule(ruleToForm({ status: 'Enabled', prefix: 'old/', expiration: { days: 1 } }))
    expect(back.filter).toEqual({ prefix: 'old/' })
    expect(back.prefix).toBeUndefined()
  })

  it('maps expiration dates (midnight UTC) and delete markers', () => {
    expect(isoToDateInput('2027-01-01T00:00:00.000Z')).toBe('2027-01-01')
    expect(dateInputToIso('2027-01-01')).toBe('2027-01-01T00:00:00.000Z')
    const f = { ...newRuleForm(), expirationMode: 'date' as const, expirationDate: '2027-01-01', abortDays: null }
    expect(formToRule(f).expiration).toEqual({ date: '2027-01-01T00:00:00.000Z' })
    expect(ruleToForm(formToRule(f)).expirationDate).toBe('2027-01-01')
    const dm = { ...newRuleForm(), expirationMode: 'none' as const, expiredObjectDeleteMarker: true, abortDays: null }
    expect(formToRule(dm).expiration).toEqual({ expiredObjectDeleteMarker: true })
    expect(ruleToForm(formToRule(dm)).expiredObjectDeleteMarker).toBe(true)
  })

  it('never emits ExpiredObjectDeleteMarker together with days', () => {
    const f = { ...newRuleForm(), expirationMode: 'days' as const, expirationDays: 5, expiredObjectDeleteMarker: true }
    expect(formToRule(f).expiration).toEqual({ days: 5 })
  })

  it('survives an XML round trip through the S3 builder and parser', () => {
    const f = { ...newRuleForm(), prefix: 'tmp/', sizeLessThan: 2048, noncurrentDays: 3 }
    const rule = formToRule(f)
    const [parsed] = parseLifecycle(buildLifecycle([rule]))
    expect(ruleToForm(parsed!)).toMatchObject({ prefix: 'tmp/', sizeLessThan: 2048, expirationDays: 30, noncurrentDays: 3, abortDays: 7 })
  })

  it('creates unique default ids', () => {
    expect(newRuleForm(['rule-2', 'x']).id).toBe('rule-3')
    expect(newRuleForm(['rule-1']).id).toBe('rule-2')
  })
})

describe('lifecycle validation', () => {
  it('accepts the default rule', () => {
    expect(validateRuleForm(newRuleForm())).toEqual({})
  })

  it('requires an action and valid numbers', () => {
    const f = { ...newRuleForm(), expirationMode: 'none' as const, abortDays: null }
    expect(validateRuleForm(f).actions).toBeDefined()
    expect(validateRuleForm({ ...newRuleForm(), expirationDays: 0 }).expirationDays).toBeDefined()
    expect(validateRuleForm({ ...newRuleForm(), expirationMode: 'date', expirationDate: '' }).expirationDate).toBeDefined()
    expect(validateRuleForm({ ...newRuleForm(), noncurrentDays: 1.5 }).noncurrentDays).toBeDefined()
    expect(validateRuleForm({ ...newRuleForm(), newerNoncurrentVersions: 2 }).newerNoncurrentVersions).toMatch(/noncurrent days/)
  })

  it('enforces S3 combination rules', () => {
    const tagged = { ...newRuleForm(), tags: [{ key: 'a', value: '1' }] }
    expect(validateRuleForm(tagged).abortDays).toMatch(/tag/)
    expect(validateRuleForm({ ...tagged, abortDays: null, expirationMode: 'none', expiredObjectDeleteMarker: true }).expiredObjectDeleteMarker).toMatch(/tag/)
    expect(validateRuleForm({ ...newRuleForm(), expiredObjectDeleteMarker: true }).expiredObjectDeleteMarker).toMatch(/days/)
    expect(validateRuleForm({ ...newRuleForm(), sizeGreaterThan: 10, sizeLessThan: 5 }).size).toBeDefined()
    expect(validateRuleForm({ ...newRuleForm(), tags: [{ key: 'a', value: '1' }, { key: 'a', value: '2' }], abortDays: null }).tags).toMatch(/unique/)
    expect(validateRuleForm({ ...newRuleForm(), id: 'x' }, ['x']).id).toBeDefined()
  })

  it('describes rules for the list', () => {
    const rule = formToRule({ ...newRuleForm(), prefix: 'logs/', noncurrentDays: 1, newerNoncurrentVersions: 2 })
    expect(describeFilter(rule)).toBe('prefix "logs/"')
    expect(describeFilter({ status: 'Enabled', filter: { prefix: '' } })).toBe('Whole bucket')
    expect(describeActions(rule)).toEqual([
      'Expire after 30 days',
      'Delete noncurrent versions after 1 day (keep 2 newest)',
      'Abort incomplete uploads after 7 days'
    ])
  })
})

describe('CORS rule model', () => {
  it('normalises lists and orders methods', () => {
    const r = formToCorsRule({ id: ' ', allowedOrigins: [' https://a.example ', ''], allowedMethods: ['PUT', 'GET', 'BOGUS'], allowedHeaders: ['*'], exposeHeaders: ['ETag', 'ETag'], maxAgeSeconds: 10 })
    expect(r).toEqual({ id: undefined, allowedOrigins: ['https://a.example'], allowedMethods: ['GET', 'PUT'], allowedHeaders: ['*'], exposeHeaders: ['ETag'], maxAgeSeconds: 10 })
    expect(corsRuleToForm(r).maxAgeSeconds).toBe(10)
    expect(formToCorsRule({ ...newCorsRule(), maxAgeSeconds: null }).maxAgeSeconds).toBeUndefined()
  })

  it('validates rules', () => {
    expect(hasErrors(validateCorsRule(newCorsRule()))).toBe(false)
    const e = validateCorsRule({ id: '', allowedOrigins: [], allowedMethods: [], allowedHeaders: ['a**'], exposeHeaders: ['bad header'], maxAgeSeconds: -1 })
    expect(Object.keys(e).sort()).toEqual(['allowedHeaders', 'allowedMethods', 'allowedOrigins', 'exposeHeaders', 'maxAgeSeconds'])
    expect(validateCorsRule({ ...newCorsRule(), allowedOrigins: ['https://*.*.x'] }).allowedOrigins).toBeDefined()
  })
})

describe('public access block XML', () => {
  it('builds and parses the configuration', () => {
    const cfg = { blockPublicAcls: true, ignorePublicAcls: false, blockPublicPolicy: true, restrictPublicBuckets: false }
    const xml = buildPublicAccessBlock(cfg)
    expect(xml).toContain('<BlockPublicAcls>true</BlockPublicAcls>')
    expect(xml).toContain('<IgnorePublicAcls>false</IgnorePublicAcls>')
    expect(parsePublicAccessBlock(xml)).toEqual(cfg)
  })
})

describe('bucket compression vs the server mode', () => {
  it('flags only the switch direction the server refuses (409)', () => {
    // enforced: turning it off is refused, turning it on is fine
    expect(compressionConflict('enforced', true)).toMatch(/enforces compression/)
    expect(compressionConflict('enforced', false)).toBeUndefined()
    // disabled: turning it on is refused, turning it off is fine
    expect(compressionConflict('disabled', false)).toMatch(/disabled compression/)
    expect(compressionConflict('disabled', true)).toBeUndefined()
    // optional: the bucket decides
    expect(compressionConflict('optional', true)).toBeUndefined()
    expect(compressionConflict('optional', false)).toBeUndefined()
    expect(compressionConflict('enforced', undefined)).toBeUndefined()
  })

  it('describes every mode, and unknown modes as they are', () => {
    for (const m of COMPRESSION_MODES) expect(COMPRESSION_MODE_TEXT[m].label).toBeTruthy()
    expect(compressionModeText('enforced').label).toBe('Enforced')
    expect(compressionModeText('adaptive').label).toBe('adaptive')
    expect(compressionModeText(undefined).label).toBe('Unknown')
  })
})
