import { describe, expect, it } from 'vitest'
import { formatBytes, formatDate, formatDuration, formatNumber, formatPercent, formatRelativeTime, formatSeconds, nsToSeconds, secondsToNs, truncateMiddle } from '~/utils/format'
import { asFolderPrefix, baseName, extName, isFolderKey, joinKey, parentPrefix, prefixBreadcrumbs, routeParamToKey, validateBucketName, validateObjectKey } from '~/utils/paths'
import { appRoutes, routeParam } from '~/utils/routes'
import { completeConnection, endpointSlug, hasCredentials, parseConnectionList, sameEndpoint, toConnectionInput } from '~/utils/connections'
import { describeError, errorMessage, isAbortedError, isAuthError } from '~/utils/errors'
import { S3Error } from '~/utils/s3/errors'
import { AdminError } from '~/utils/admin/client'

describe('format', () => {
  it('formats bytes', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(1023)).toBe('1023 B')
    expect(formatBytes(1024)).toBe('1 KiB')
    expect(formatBytes(1536)).toBe('1.5 KiB')
    expect(formatBytes(5 * 1024 ** 3)).toBe('5 GiB')
    expect(formatBytes(123.4 * 1024 ** 2)).toBe('123 MiB')
    expect(formatBytes(undefined)).toBe('—')
    expect(formatBytes(-2048)).toBe('-2 KiB')
  })

  it('formats durations from nanoseconds (Go time.Duration)', () => {
    expect(formatDuration(0)).toBe('0s')
    expect(formatDuration(500)).toBe('500ns')
    expect(formatDuration(1500)).toBe('1.5µs')
    expect(formatDuration(2_500_000)).toBe('2.5ms')
    expect(formatDuration(1_500_000_000)).toBe('1.5s')
    expect(formatDuration(10e9)).toBe('10s')
    expect(formatDuration(60e9)).toBe('1m')
    expect(formatDuration(3725e9)).toBe('1h 2m 5s')
    expect(formatDuration(null)).toBe('—')
    expect(secondsToNs(1.5)).toBe(1_500_000_000)
    expect(nsToSeconds(10e9)).toBe(10)
  })

  it('formats seconds, percent, numbers, relative time', () => {
    expect(formatSeconds(90061)).toBe('1d 1h 1m')
    expect(formatSeconds(0)).toBe('0s')
    expect(formatPercent(25, 200)).toBe('12.5%')
    expect(formatPercent(1, 0)).toBe('0%')
    expect(formatNumber(1234567)).toMatch(/1.234.567/)
    const now = Date.parse('2026-10-09T12:00:00Z')
    expect(formatRelativeTime('2026-10-09T11:57:00Z', now)).toMatch(/3 minutes ago/)
    expect(formatRelativeTime('2026-10-09T12:00:02Z', now)).toBe('just now')
    expect(formatRelativeTime('2026-10-11T12:00:00Z', now)).toMatch(/in 2 days/)
    expect(formatRelativeTime('garbage', now)).toBe('—')
    expect(formatDate('')).toBe('—')
    expect(truncateMiddle('abcdefghij', 5)).toBe('ab…ij')
  })
})

describe('paths', () => {
  it('splits prefixes into breadcrumbs', () => {
    expect(prefixBreadcrumbs('')).toEqual([])
    expect(prefixBreadcrumbs('a/b/c/')).toEqual([
      { label: 'a', prefix: 'a/' },
      { label: 'b', prefix: 'a/b/' },
      { label: 'c', prefix: 'a/b/c/' }
    ])
    expect(prefixBreadcrumbs('a/b')).toEqual([{ label: 'a', prefix: 'a/' }, { label: 'b', prefix: 'a/b/' }])
  })

  it('joins and splits keys', () => {
    expect(joinKey('a/', '/b', 'c.txt')).toBe('a/b/c.txt')
    expect(joinKey('', 'a', 'b/')).toBe('a/b/')
    expect(joinKey()).toBe('')
    expect(parentPrefix('a/b/c.txt')).toBe('a/b/')
    expect(parentPrefix('a/b/')).toBe('a/')
    expect(parentPrefix('a')).toBe('')
    expect(baseName('a/b/c.txt')).toBe('c.txt')
    expect(baseName('a/b/')).toBe('b')
    expect(extName('x/archive.TAR.GZ')).toBe('gz')
    expect(extName('.hidden')).toBe('')
    expect(isFolderKey('a/')).toBe(true)
    expect(asFolderPrefix('a')).toBe('a/')
    expect(asFolderPrefix('')).toBe('')
    expect(routeParamToKey(['a', 'b c', 'd.txt'])).toBe('a/b c/d.txt')
    expect(routeParamToKey(undefined)).toBe('')
  })

  it('validates names', () => {
    expect(validateBucketName('my-bucket')).toBeUndefined()
    expect(validateBucketName('ab')).toBeDefined()
    expect(validateBucketName('My_Bucket')).toBeDefined()
    expect(validateBucketName('192.168.1.1')).toBeDefined()
    expect(validateBucketName('a..b')).toBeDefined()
    expect(validateBucketName('xn--abc')).toBeDefined()
    expect(validateBucketName('-abc')).toBeDefined()
    expect(validateObjectKey('ok/key.txt')).toBeUndefined()
    expect(validateObjectKey('')).toBeDefined()
    expect(validateObjectKey('x'.repeat(1025))).toBeDefined()
  })
})

describe('routes', () => {
  it('builds canonical app routes', () => {
    expect(appRoutes.server('abc')).toBe('/servers/abc')
    expect(appRoutes.bucket('abc', 'photos', 'a b/')).toBe('/servers/abc/buckets/photos?prefix=a%20b%2F')
    expect(appRoutes.object('abc', 'photos', 'a b/c#.txt', 'v1')).toBe('/servers/abc/buckets/photos/objects/a%20b/c%23.txt?versionId=v1')
    expect(appRoutes.connect('abc', '/servers/abc/users')).toBe('/connect?server=abc&redirect=%2Fservers%2Fabc%2Fusers')
    expect(appRoutes.connect()).toBe('/connect')
    expect(appRoutes.user('x', 'a/b')).toBe('/servers/x/users/a%2Fb')
    expect(routeParam(['a', 'b'])).toBe('a')
    expect(routeParam(undefined)).toBe('')
  })
})

describe('connection helpers', () => {
  it('parses lenient connection lists', () => {
    expect(parseConnectionList('')).toEqual([])
    expect(parseConnectionList({ endpoint: 'h:1' })).toHaveLength(1)
    expect(toConnectionInput({ url: 'ftp://x' })).toBeUndefined()
    expect(toConnectionInput('str')).toBeUndefined()
    expect(endpointSlug('https://S3.Example.com:443/base')).toBe('s3-example-com-443-base')
    expect(sameEndpoint('http://a:1/', 'HTTP://A:1')).toBe(true)
    expect(hasCredentials({ accessKeyId: 'a', secretAccessKey: '' })).toBe(false)
    expect(completeConnection({ endpoint: 'h:1' }, { region: 'x' }).region).toBe('x')
  })
})

describe('error helpers', () => {
  it('describes S3 and admin errors', () => {
    const s3 = new S3Error({ code: 'SignatureDoesNotMatch', message: 'bad sig', status: 403, requestId: 'R' })
    expect(describeError(s3)).toMatchObject({ title: 'SignatureDoesNotMatch (403)', requestId: 'R' })
    expect(describeError(s3).hint).toContain('secret key')
    expect(isAuthError(s3)).toBe(true)
    const admin = new AdminError('Unauthenticated', 'InvalidAccessKeyId', 401)
    expect(describeError(admin).hint).toContain('access key')
    expect(isAuthError(admin)).toBe(true)
    expect(errorMessage('boom')).toBe('boom')
    expect(isAbortedError(new DOMException('x', 'AbortError'))).toBe(true)
    expect(isAbortedError(new Error('x'))).toBe(false)
  })
})
