import { describe, expect, it } from 'vitest'
import type { Capabilities, MinuteBucket, OpStats } from '~/types/admin'
import {
  BUCKET_TABS,
  canCreateOwnKeys,
  canListUsers,
  canForceDeleteBucket,
  canManageOwnKeys,
  canProbe,
  canShowBucketUsage,
  canSimulate,
  hasCapability,
  NO_CAPS,
  overviewMode,
  policiesPageMode,
  resolveBucketTab,
  settingsPageMode,
  usersPageMode
} from '~/components/admin/-capabilities'
import {
  areaPath,
  diskUsedPercent,
  fillMinutes,
  formatLatencyMs,
  indexAtX,
  linePath,
  nextSort,
  niceBytesMax,
  niceMax,
  opErrorRate,
  sortOps,
  xAt,
  yAt,
  type ChartBox
} from '~/components/admin/-metrics'

const caps = (over: Partial<Capabilities> = {}): Capabilities => ({ ...NO_CAPS, ...over })
const ALL = caps({ admin: true, viewInfo: true, viewStats: true, manageConfig: true, manageUsers: true, managePolicies: true, manageBuckets: true, discovery: true, scan: true })

describe('capability gating', () => {
  it('never grants anything to generic S3 connections', () => {
    expect(hasCapability('s3', ALL, 'manageUsers')).toBe(false)
    expect(overviewMode('s3', ALL)).toBe('s3')
    expect(usersPageMode('s3', ALL)).toBe('unavailable')
    expect(policiesPageMode('s3', ALL)).toBe('unavailable')
    expect(settingsPageMode('s3', ALL)).toBe('unavailable')
    expect(canSimulate('s3', ALL)).toBe(false)
    expect(canForceDeleteBucket('s3', ALL)).toBe(false)
    expect(canShowBucketUsage('s3', ALL)).toBe(false)
    expect(canManageOwnKeys('s3', false, true)).toBe(false)
  })

  it('selects the overview mode from stats/info capabilities', () => {
    expect(overviewMode('gos3', ALL)).toBe('stats')
    expect(overviewMode('gos3', caps({ viewInfo: true }))).toBe('info')
    expect(overviewMode('gos3', NO_CAPS)).toBe('public')
    expect(overviewMode('gos3', null)).toBe('public')
  })

  it('shows user management or self-service keys', () => {
    expect(usersPageMode('gos3', caps({ manageUsers: true }))).toBe('manage')
    expect(usersPageMode('gos3', NO_CAPS)).toBe('self')
    // A delegated manager (user actions scoped to some users) lists users
    // without service-level admin:CreateUser.
    expect(usersPageMode('gos3', caps({ listUsers: true }))).toBe('manage')
    expect(canManageOwnKeys('gos3', true, true)).toBe(false)
    expect(canManageOwnKeys('gos3', false, true)).toBe(true)
    expect(canManageOwnKeys('gos3', false, false)).toBe(false)
  })

  it('lists users with admin:ListUsers, falling back to manageUsers on older servers', () => {
    expect(canListUsers(caps({ listUsers: true }))).toBe(true)
    expect(canListUsers(caps({ listUsers: false, manageUsers: true }))).toBe(false)
    const { listUsers: _omit, ...older } = caps({ manageUsers: true })
    expect(canListUsers(older)).toBe(true)
    expect(canListUsers({ ...older, manageUsers: false })).toBe(false)
    expect(canListUsers(null)).toBe(false)
  })

  it('distinguishes manage / readonly / unavailable for policies and settings', () => {
    expect(policiesPageMode('gos3', caps({ managePolicies: true }))).toBe('manage')
    expect(policiesPageMode('gos3', caps({ admin: true }))).toBe('readonly')
    expect(policiesPageMode('gos3', NO_CAPS)).toBe('unavailable')
    expect(settingsPageMode('gos3', caps({ manageConfig: true }))).toBe('manage')
    expect(settingsPageMode('gos3', caps({ admin: true, viewStats: true }))).toBe('readonly')
  })

  it('gates simulator, force delete and bucket usage', () => {
    expect(canSimulate('gos3', caps({ manageUsers: true }))).toBe(true)
    expect(canSimulate('gos3', caps({ managePolicies: true }))).toBe(true)
    expect(canSimulate('gos3', caps({ admin: true }))).toBe(false)
    expect(canForceDeleteBucket('gos3', caps({ manageBuckets: true }))).toBe(true)
    expect(canForceDeleteBucket('gos3', caps({ admin: true }))).toBe(false)
    expect(canShowBucketUsage('gos3', caps({ admin: true }))).toBe(true)
    expect(canShowBucketUsage('gos3', NO_CAPS)).toBe(false)
  })

  it('gates probing on admin:ProbeNetwork (probe), falling back to discovery for older servers', () => {
    expect(canProbe('gos3', caps({ discovery: true, probe: true }))).toBe(true)
    // diagnostics: reads discovery, may not probe
    expect(canProbe('gos3', caps({ discovery: true, probe: false }))).toBe(false)
    expect(canProbe('gos3', caps({ probe: true }))).toBe(true)
    expect(canProbe('s3', caps({ probe: true }))).toBe(false)
    expect(canProbe('gos3', null)).toBe(false)
    // server without the field: it still gated probes on admin:GetDiscovery
    const { probe: _p, ...old } = caps({ discovery: true })
    expect(canProbe('gos3', old)).toBe(true)
    const { probe: _q, ...oldNoDiscovery } = caps()
    expect(canProbe('gos3', oldNoDiscovery)).toBe(false)
  })

  it('gates self-service key creation on admin:CreateOwnAccessKey (createOwnKeys)', () => {
    expect(canCreateOwnKeys('gos3', false, caps({ createOwnKeys: true }))).toBe(true)
    expect(canCreateOwnKeys('gos3', false, caps({ createOwnKeys: false }))).toBe(false)
    expect(canCreateOwnKeys('gos3', true, caps({ createOwnKeys: true }))).toBe(false) // root has no IAM keys
    expect(canCreateOwnKeys('s3', false, caps({ createOwnKeys: true }))).toBe(false)
    expect(canCreateOwnKeys('gos3', false, null)).toBe(false)
    // server without the field allowed it unless explicitly denied
    const { createOwnKeys: _c, ...old } = caps()
    expect(canCreateOwnKeys('gos3', false, old)).toBe(true)
    expect(NO_CAPS).toMatchObject({ probe: false, createOwnKeys: false })
  })

  it('resolves bucket settings tabs from the query', () => {
    expect(resolveBucketTab('lifecycle')).toBe('lifecycle')
    expect(resolveBucketTab(['cors'])).toBe('cors')
    expect(resolveBucketTab('nope')).toBe('overview')
    expect(resolveBucketTab(undefined)).toBe('overview')
    expect(new Set(BUCKET_TABS.map(t => t.value)).size).toBe(BUCKET_TABS.length)
  })
})

describe('traffic series', () => {
  const bucket = (iso: string, requests: number): MinuteBucket => ({ minute: iso, requests, errors: 0, bytesIn: 0, bytesOut: 0 })

  it('fills 60 consecutive minutes ending at the newest bucket', () => {
    const pts = fillMinutes([bucket('2026-10-09T10:00:00Z', 5), bucket('2026-10-09T10:02:00Z', 7)], Date.parse('2026-10-09T09:00:00Z'))
    expect(pts).toHaveLength(60)
    expect(pts[59]!.minute).toBe('2026-10-09T10:02:00.000Z')
    expect(pts[59]!.requests).toBe(7)
    expect(pts[58]!.requests).toBe(0)
    expect(pts[57]!.requests).toBe(5)
    expect(pts[0]!.minute).toBe('2026-10-09T09:03:00.000Z')
  })

  it('handles null / empty input', () => {
    const pts = fillMinutes(null, Date.parse('2026-10-09T10:00:30Z'), 3)
    expect(pts.map(p => p.minute)).toEqual(['2026-10-09T09:58:00.000Z', '2026-10-09T09:59:00.000Z', '2026-10-09T10:00:00.000Z'])
    expect(pts.every(p => p.requests === 0)).toBe(true)
  })

  it('computes nice axis maxima', () => {
    expect(niceMax(0)).toBe(1)
    expect(niceMax(7)).toBe(10)
    expect(niceMax(13)).toBe(20)
    expect(niceMax(230)).toBe(250)
    expect(niceMax(0.3)).toBeCloseTo(0.5)
    expect(niceBytesMax(0)).toBe(1024)
    expect(niceBytesMax(1500)).toBe(2048)
    expect(niceBytesMax(3 * 1024 * 1024)).toBe(5 * 1024 * 1024)
  })

  it('maps values to SVG coordinates', () => {
    const box: ChartBox = { width: 110, height: 60, padLeft: 10, padRight: 0, padTop: 0, padBottom: 10 }
    expect(xAt(0, 11, box)).toBe(10)
    expect(xAt(10, 11, box)).toBe(110)
    expect(yAt(0, 10, box)).toBe(50)
    expect(yAt(10, 10, box)).toBe(0)
    expect(yAt(20, 10, box)).toBe(0)
    expect(linePath([0, 10], 10, box)).toBe('M10 50 L110 0')
    expect(areaPath([0, 10], 10, box)).toBe('M10 50 L110 0 L110 50 L10 50 Z')
    expect(linePath([], 10, box)).toBe('')
    expect(indexAtX(60, 11, box)).toBe(5)
    expect(indexAtX(-100, 11, box)).toBe(0)
    expect(indexAtX(1000, 11, box)).toBe(10)
  })
})

describe('per-operation table', () => {
  const op = (name: string, requests: number, e4: number, p99: number): OpStats => ({
    op: name, requests, errors4xx: e4, errors5xx: 0, bytesIn: 0, bytesOut: 0, avgLatencyMs: 1, p50LatencyMs: 1, p99LatencyMs: p99
  })
  const ops = [op('PutObject', 10, 1, 50), op('GetObject', 100, 0, 5), op('ListObjectsV2', 10, 5, 20)]

  it('sorts by numeric and text keys without mutating the input', () => {
    expect(sortOps(ops, 'requests', 'desc').map(o => o.op)).toEqual(['GetObject', 'ListObjectsV2', 'PutObject'])
    expect(sortOps(ops, 'op', 'asc').map(o => o.op)).toEqual(['GetObject', 'ListObjectsV2', 'PutObject'])
    expect(sortOps(ops, 'p99LatencyMs', 'asc').map(o => o.op)).toEqual(['GetObject', 'ListObjectsV2', 'PutObject'])
    expect(sortOps(ops, 'errorRate', 'desc').map(o => o.op)).toEqual(['ListObjectsV2', 'PutObject', 'GetObject'])
    expect(ops[0]!.op).toBe('PutObject')
    expect(sortOps(null, 'op', 'asc')).toEqual([])
  })

  it('toggles sort direction', () => {
    expect(nextSort({ key: 'requests', dir: 'desc' }, 'requests')).toEqual({ key: 'requests', dir: 'asc' })
    expect(nextSort({ key: 'requests', dir: 'desc' }, 'op')).toEqual({ key: 'op', dir: 'asc' })
    expect(nextSort({ key: 'op', dir: 'asc' }, 'p99LatencyMs')).toEqual({ key: 'p99LatencyMs', dir: 'desc' })
  })

  it('formats latencies, error rates and disk usage', () => {
    expect(formatLatencyMs(0.4231)).toBe('0.42 ms')
    expect(formatLatencyMs(12.4)).toBe('12 ms')
    expect(formatLatencyMs(1500)).toBe('1.5 s')
    expect(formatLatencyMs(undefined)).toBe('—')
    expect(opErrorRate(ops[2]!)).toBe(0.5)
    expect(opErrorRate(op('x', 0, 0, 0))).toBe(0)
    expect(diskUsedPercent(25, 100)).toBe(25)
    expect(diskUsedPercent(1, 0)).toBe(0)
  })
})

describe('identityPending', () => {
  it('is pending only for gos3 connections whose identity is not resolved yet', async () => {
    const { identityPending } = await import('~/components/admin/-capabilities')
    expect(identityPending('gos3', undefined)).toBe(true)
    expect(identityPending('gos3', { state: 'unknown', whoami: null })).toBe(true)
    expect(identityPending('gos3', { state: 'checking', whoami: null })).toBe(true)
    expect(identityPending('gos3', { state: 'checking', whoami: {} })).toBe(false)
    expect(identityPending('gos3', { state: 'unauthorized', whoami: null })).toBe(false)
    expect(identityPending('gos3', { state: 'online', whoami: null })).toBe(false)
    expect(identityPending('s3', undefined)).toBe(false)
  })
})
