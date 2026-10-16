// The bucket's "Backup & archive" tab: the restic admin client calls, the
// capability gating, the archive toggle, shell quoting and recovery
// snippets (bash and PowerShell), the snapshot schedule form (validation,
// retention presets, request body) and the restore dialog.
import { spawnSync } from 'node:child_process'
import { describe, expect, it, vi } from 'vitest'
import { AdminClient } from '~/utils/admin/client'
import type { Capabilities, ResticTags, SnapshotConfig } from '~/types/admin'
import { NO_CAPS, bucketSettingsTabs, resolveBucketTab, resticAccess, resticTabAvailable } from '~/components/admin/-capabilities'
import {
  ARCHIVE_COST_WARNING,
  RETENTION_PRESETS,
  archiveAvailability,
  archiveEnabled,
  archiveToggleBody,
  availableProviders,
  bashQuote,
  bucketLacksRepository,
  defaultSnapshotForm,
  describeRetention,
  describeSchedule,
  describeTarget,
  gos3ResticCommand,
  GOS3_CONFIG_PATH,
  isPartialSnapshot,
  isSecretEnv,
  maskCredentials,
  MASK,
  NATIVE_NO_PROVIDER,
  emptyRestoreForm,
  envLine,
  powershellQuote,
  presetRetention,
  resticSnippet,
  restoreRequest,
  retentionPreset,
  snapshotConfigFrom,
  snapshotFormDirty,
  snapshotFormFrom,
  snippetFromCredentials,
  snippetFromInfo,
  sortSnapshots,
  validateRestoreForm,
  validateSnapshotForm
} from '~/components/bucket-settings/-restic'

interface Captured { method: string, url: string, body: string }

function makeAdmin(responder: (c: Captured) => Response) {
  const calls: Captured[] = []
  const fetch = vi.fn(async (req: Request) => {
    const body = ['GET', 'HEAD'].includes(req.method) ? '' : await req.text()
    const c = { method: req.method, url: req.url.replace('http://localhost:9000/_gos3/admin/v1', ''), body }
    calls.push(c)
    return responder(c)
  })
  const admin = new AdminClient({ endpoint: 'http://localhost:9000', accessKeyId: 'AK', secretAccessKey: 'SK' }, { fetch, datetime: '20261009T120000Z' })
  return { admin, calls }
}

const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status, headers: { 'content-type': 'application/json' } })

const TAGS: ResticTags = { object: 'gos3-object', bucket: 'gos3-bucket:cGhvdG9z', bucketId: 'gos3-bucket-id:b1', snapshot: 'gos3-snapshot' }

const CONFIG: SnapshotConfig = {
  enabled: true,
  interval: '24h0m0s',
  at: '03:00',
  consistent: true,
  prefix: '',
  retention: { keepLast: 7, keepHourly: 0, keepDaily: 7, keepWeekly: 4, keepMonthly: 12, keepYearly: 0, keepWithin: '0s' },
  target: { kind: 'backing', backing: 'cold', repo: 'snap-1a2b' },
  prune: { enabled: false, interval: '168h0m0s' }
}

describe('AdminClient restic', () => {
  it('sets the provider with a scope and reads the recovery information', async () => {
    const { admin, calls } = makeAdmin(c => json(c.url.endsWith('/restic') ? { settings: null, archive: null, snapshots: null, snapshotRepository: null, tags: TAGS } : { provider: 'restic', scope: 'all' }))
    await admin.putBucketEncryptionProvider('photos', { provider: 'restic', scope: 'all' })
    expect(calls[0]).toMatchObject({ method: 'PUT', url: '/buckets/photos/encryption-provider', body: '{"provider":"restic","scope":"all"}' })
    expect((await admin.getBucketRestic('my photos')).tags.object).toBe('gos3-object')
    expect(calls[1]).toMatchObject({ method: 'GET', url: '/buckets/my%20photos/restic' })
  })

  it('reveals credentials with backing and purpose', async () => {
    const creds = { backing: 'default', purpose: 'archive', repository: '/data/r', password: 'pw', env: null, options: null }
    const { admin, calls } = makeAdmin(() => json(creds))
    expect(await admin.getResticCredentials('photos', { backing: 'default', purpose: 'archive' })).toEqual(creds)
    expect(calls[0]).toMatchObject({ method: 'POST', url: '/buckets/photos/restic/credentials', body: '{"backing":"default","purpose":"archive"}' })
    await admin.getResticCredentials('photos')
    expect(calls[1]!.body).toBe('{}')
  })

  it('reads, saves and removes the snapshot schedule, runs, forgets, restores and prunes', async () => {
    const job = { id: 'j1', kind: 'restic-snapshot', state: 'queued', createdAt: '2026-10-15T10:00:00Z' }
    const { admin, calls } = makeAdmin((c) => {
      if (c.method === 'DELETE') return new Response(null, { status: 204 })
      if (c.url.endsWith('/config')) return json(CONFIG)
      if (c.method === 'POST') return json(job, 202)
      return json({ config: null, status: {}, repository: null, snapshots: null })
    })
    expect((await admin.getBucketSnapshots('photos')).snapshots).toEqual([])
    expect(await admin.putSnapshotConfig('photos', CONFIG)).toEqual(CONFIG)
    await admin.deleteSnapshotConfig('photos')
    expect((await admin.runBucketSnapshot('photos')).id).toBe('j1')
    await admin.forgetSnapshot('photos', 'ab12')
    await admin.restoreSnapshot('photos', 'ab12', { target: { bucket: 'r', create: {} }, configs: 'safe' })
    await admin.pruneBucketSnapshots('photos')
    expect(calls.map(c => `${c.method} ${c.url}`)).toEqual([
      'GET /buckets/photos/restic/snapshots',
      'PUT /buckets/photos/restic/snapshots/config',
      'DELETE /buckets/photos/restic/snapshots/config',
      'POST /buckets/photos/restic/snapshots',
      'DELETE /buckets/photos/restic/snapshots/ab12',
      'POST /buckets/photos/restic/snapshots/ab12/restore',
      'POST /buckets/photos/restic/prune'
    ])
    expect(JSON.parse(calls[1]!.body)).toEqual(CONFIG)
    expect(JSON.parse(calls[5]!.body)).toEqual({ target: { bucket: 'r', create: {} }, configs: 'safe' })
  })
})

describe('restic capabilities and tab', () => {
  const caps = (over: Partial<Capabilities> = {}): Capabilities => ({ ...NO_CAPS, ...over })

  it('derives what the tab offers from the capabilities', () => {
    expect(resticAccess('gos3', caps())).toEqual({ snapshots: false, run: false, restore: false, reveal: false })
    expect(resticAccess('gos3', caps({ manageSnapshots: true }))).toEqual({ snapshots: true, run: false, restore: false, reveal: false })
    expect(resticAccess('gos3', caps({ runSnapshots: true }))).toMatchObject({ run: true, snapshots: false })
    // Restoring needs RestoreBucketSnapshot and CreateCopyJob on the target.
    expect(resticAccess('gos3', caps({ restoreSnapshots: true })).restore).toBe(false)
    expect(resticAccess('gos3', caps({ manageJobs: true })).restore).toBe(false)
    expect(resticAccess('gos3', caps({ restoreSnapshots: true, manageJobs: true })).restore).toBe(true)
    expect(resticAccess('gos3', caps({ revealResticCredentials: true })).reveal).toBe(true)
    expect(resticAccess('s3', caps({ manageSnapshots: true, runSnapshots: true, restoreSnapshots: true, manageJobs: true, revealResticCredentials: true })))
      .toEqual({ snapshots: false, run: false, restore: false, reveal: false })
    // Older servers without the fields offer none of them.
    const { runSnapshots: _r, restoreSnapshots: _s, ...older } = caps({ manageSnapshots: true, manageJobs: true })
    expect(resticAccess('gos3', older)).toEqual({ snapshots: true, run: false, restore: false, reveal: false })
  })

  it('shows the Backup & archive tab on gos3 servers that report manageSnapshots', () => {
    expect(resticTabAvailable('gos3', caps())).toBe(true)
    const { manageSnapshots: _m, ...old } = caps()
    expect(resticTabAvailable('gos3', old)).toBe(false)
    expect(resticTabAvailable('s3', caps({ manageSnapshots: true }))).toBe(false)
    const tabs = bucketSettingsTabs('gos3', caps()).map(t => t.value)
    expect(tabs.slice(tabs.indexOf('encryption'), tabs.indexOf('encryption') + 2)).toEqual(['encryption', 'restic'])
    expect(bucketSettingsTabs('gos3', caps()).find(t => t.value === 'restic')!.label).toBe('Backup & archive')
    expect(bucketSettingsTabs('gos3', old).map(t => t.value)).not.toContain('restic')
    expect(resolveBucketTab('restic', bucketSettingsTabs('gos3', old))).toBe('overview')
    expect(resolveBucketTab('restic', bucketSettingsTabs('gos3', caps()))).toBe('restic')
  })
})

describe('archive toggle', () => {
  it('is on for restic with scope all only', () => {
    expect(archiveEnabled({ provider: 'restic', scope: 'all' })).toBe(true)
    expect(archiveEnabled({ provider: 'restic', scope: 'sse' })).toBe(false)
    expect(archiveEnabled({ provider: 'restic' })).toBe(false)
    expect(archiveEnabled({ provider: 'AES256', scope: 'all' })).toBe(false)
    expect(archiveEnabled(null)).toBe(false)
  })

  it('is available when restic is listed; only a missing list (older servers) means unknown', () => {
    expect(archiveAvailability({ available: ['AES256', 'restic'] })).toEqual({ available: true, reason: '' })
    expect(archiveAvailability({})).toEqual({ available: true, reason: '' })
    const no = archiveAvailability({ available: [] })
    expect(no.available).toBe(false)
    expect(no.reason).toMatch(/restic/i)
    expect(archiveAvailability({ available: ['AES256'] }).available).toBe(false)
    expect(archiveAvailability(undefined).available).toBe(false)
    // Native buckets never, whatever the document says.
    expect(archiveAvailability({ available: ['restic'] }, 'native')).toEqual({ available: false, reason: NATIVE_NO_PROVIDER })
    expect(archiveAvailability({}, 'native').available).toBe(false)
    expect(archiveAvailability({ available: ['restic'] }, 'wrapped').available).toBe(true)
  })

  it('lists the available providers', () => {
    expect(availableProviders({ available: [] })).toEqual([])
    expect(availableProviders({ available: ['AES256'] })).toEqual(['AES256'])
    expect(availableProviders({})).toEqual(['AES256', 'restic'])
    expect(availableProviders(null)).toEqual(['AES256', 'restic'])
    expect(availableProviders({ available: ['AES256', 'restic'] }, 'native')).toEqual([])
  })

  it('needs a backing for snapshots only for buckets without a repository of their own', () => {
    const copy = (over: object) => ({ backing: 'default', role: 'primary', ...over })
    const info = { repository: '/r', passwordSource: 'p', initialized: true }
    expect(bucketLacksRepository([copy({ info })])).toBe(false)
    // Never written with restic yet: still the bucket's own repository.
    expect(bucketLacksRepository([copy({ info: null, error: 'not created yet' })])).toBe(false)
    expect(bucketLacksRepository([copy({ info: null, error: 'no repository' })])).toBe(true)
    // The primary decides, not a replica.
    expect(bucketLacksRepository([copy({ info }), copy({ backing: 'mem', role: 'replica', error: 'no repository' })])).toBe(false)
    expect(bucketLacksRepository([copy({ info })], 'native')).toBe(true)
    expect(bucketLacksRepository(null)).toBe(false)
  })

  it('turns restic on for every write, and back to SSE writes only', () => {
    expect(archiveToggleBody(true)).toEqual({ provider: 'restic', scope: 'all' })
    expect(archiveToggleBody(false)).toEqual({ provider: 'restic', scope: 'sse' })
    expect(ARCHIVE_COST_WARNING).toMatch(/Every object version/)
  })
})

describe('shell quoting', () => {
  const tricky = ["it's", 'a b', '$HOME', '`id`', '"q"', 'p$(rm -rf ~)w', 'back\\slash', 'semi;colon&amp|', '', 'new\nline', 'smart ‘quote’ ‚x‛', 'tab\tch', '*?[a]', '#hash', '~user', '!bang']

  it('quotes for bash: safe words as they are, everything else in single quotes', () => {
    expect(bashQuote('s3.bucket-lookup=path')).toBe('s3.bucket-lookup=path')
    expect(bashQuote('gos3-object,gos3-bucket:cGhvdG9z')).toBe('gos3-object,gos3-bucket:cGhvdG9z')
    expect(bashQuote('a b')).toBe("'a b'")
    expect(bashQuote("it's")).toBe("'it'\\''s'")
    expect(bashQuote('$HOME')).toBe("'$HOME'")
    expect(bashQuote('')).toBe("''")
  })

  it('quotes for PowerShell: always single quotes, doubling straight and typographic single quotes', () => {
    expect(powershellQuote('a b')).toBe("'a b'")
    expect(powershellQuote("it's")).toBe("'it''s'")
    expect(powershellQuote('$env:X `n "q"')).toBe("'$env:X `n \"q\"'")
    expect(powershellQuote('‘x’')).toBe("'‘‘x’’'")
    expect(powershellQuote('')).toBe("''")
  })

  it('sets variables per shell and refuses names the shells cannot set', () => {
    expect(envLine('bash', 'RESTIC_PASSWORD', "pa$s 'w'")).toBe("export RESTIC_PASSWORD='pa$s '\\''w'\\'''")
    expect(envLine('powershell', 'RESTIC_PASSWORD', "pa$s 'w'")).toBe("$env:RESTIC_PASSWORD = 'pa$s ''w'''")
    expect(envLine('bash', 'BAD-NAME', 'x')).toMatch(/^# skipped invalid variable name/)
    expect(envLine('powershell', '1X', 'x')).toMatch(/^# /)
  })

  // A real round trip where a POSIX shell is at hand (Linux and macOS CI).
  const shell = process.platform === 'win32' ? undefined : spawnSync('sh', ['-c', 'printf ok'], { encoding: 'utf8' })
  it.skipIf(shell?.stdout !== 'ok')('round-trips every value through sh', () => {
    for (const v of tricky) {
      const r = spawnSync('sh', ['-c', `printf '%s' ${bashQuote(v)}`], { encoding: 'utf8' })
      expect(r.stdout, JSON.stringify(v)).toBe(v)
    }
  })

  const pwsh = spawnSync('pwsh', ['-NoProfile', '-NonInteractive', '-Command', "Write-Output 'ok'"], { encoding: 'utf8', timeout: 20000 })
  it.skipIf(pwsh.stdout?.trim() !== 'ok')('round-trips every value through PowerShell', () => {
    for (const v of tricky.filter(t => !t.includes('\n'))) {
      const r = spawnSync('pwsh', ['-NoProfile', '-NonInteractive', '-Command', `[Console]::OutputEncoding = [Text.Encoding]::UTF8; [Console]::Out.Write(${powershellQuote(v)})`], { encoding: 'utf8', timeout: 20000 })
      expect(r.stdout, JSON.stringify(v)).toBe(v)
    }
  }, 120000)
})

describe('recovery snippets', () => {
  const info = { bucketId: 'b1', repository: "s3:https://s3.example.com/acme bucket/it's/restic/b1", passwordSource: 'record', initialized: true, envNames: ['AWS_SECRET_ACCESS_KEY', 'AWS_ACCESS_KEY_ID'], options: ['s3.bucket-lookup=path'] }

  it('lists archive snapshots without secrets, quoting every value (bash)', () => {
    const s = resticSnippet(snippetFromInfo('bash', 'photos', 'archive', info, TAGS, 'http://10.0.0.5:9000'))
    const lines = s.split('\n')
    expect(lines[0]).toBe("export RESTIC_REPOSITORY='s3:https://s3.example.com/acme bucket/it'\\''s/restic/b1'")
    expect(lines[1]).toMatch(/^# RESTIC_PASSWORD: reveal the credentials, or print every variable with the gos3 CLI, online/)
    // The gos3 CLI needs a source: this server online, or the config file offline.
    expect(lines.slice(2, 5)).toEqual([
      '#   gos3 restic env --bucket photos --shell bash --server http://10.0.0.5:9000',
      '# or offline, on the server:',
      '#   gos3 restic env --bucket photos --shell bash --config /etc/gos3/gos3.yaml'
    ])
    expect(lines.slice(5, 7)).toEqual(['# AWS_ACCESS_KEY_ID: set it to the value from the credentials', '# AWS_SECRET_ACCESS_KEY: set it to the value from the credentials'])
    expect(s).toContain('# or let gos3 decode the tags: gos3 restic recover --bucket photos --key KEY --server http://10.0.0.5:9000')
    expect(lines).toContain('restic snapshots -o s3.bucket-lookup=path --tag gos3-object,gos3-bucket:cGhvdG9z')
    expect(s).toMatch(/# restic restore SNAPSHOT_ID/)
    expect(s).not.toMatch(/RESTIC_PASSWORD=/)
    // Every line is a comment, an assignment or a restic command.
    for (const l of lines) expect(l).toMatch(/^(#|export |restic )/)
  })

  it('uses PowerShell syntax and quotes every argument there', () => {
    const s = resticSnippet(snippetFromInfo('powershell', 'photos', 'snapshots', info, TAGS))
    expect(s.split('\n')[0]).toBe("$env:RESTIC_REPOSITORY = 's3:https://s3.example.com/acme bucket/it''s/restic/b1'")
    expect(s).toContain("restic snapshots -o 's3.bucket-lookup=path' --tag 'gos3-snapshot,gos3-bucket:cGhvdG9z'")
    expect(s).toContain("restic restore latest -o 's3.bucket-lookup=path' --tag 'gos3-snapshot,gos3-bucket:cGhvdG9z' --host gos3-snapshot --target './gos3-restore'")
    // Without a known server URL the command names a placeholder; values are quoted for PowerShell.
    expect(s).toContain("#   gos3 restic env --bucket 'photos' --snapshots --shell 'powershell' --server 'SERVER_URL'")
    expect(s).toContain(`#   gos3 restic env --bucket 'photos' --snapshots --shell 'powershell' --config '${GOS3_CONFIG_PATH}'`)
    for (const l of s.split('\n')) expect(l).toMatch(/^(#|\$env:|restic )/)
  })

  it('fills revealed credentials in, with tricky values quoted', () => {
    const creds = { backing: 'default', purpose: 'archive', repository: '/data/.gos3/restic/b1', password: "p$w'd \"x\" `y`", env: { AWS_ACCESS_KEY_ID: 'AKIA', AWS_SECRET_ACCESS_KEY: 'se/cr+et$1' }, options: null }
    const bash = resticSnippet(snippetFromCredentials('bash', 'photos', creds, TAGS))
    expect(bash).toContain("export RESTIC_PASSWORD='p$w'\\''d \"x\" `y`'")
    expect(bash).toContain("export AWS_SECRET_ACCESS_KEY='se/cr+et$1'")
    expect(bash).toContain('export AWS_ACCESS_KEY_ID=AKIA')
    expect(bash).toContain('export RESTIC_REPOSITORY=/data/.gos3/restic/b1')
    const ps = resticSnippet(snippetFromCredentials('powershell', 'photos', creds, TAGS))
    expect(ps).toContain("$env:RESTIC_PASSWORD = 'p$w''d \"x\" `y`'")
    expect(ps).toContain("$env:AWS_SECRET_ACCESS_KEY = 'se/cr+et$1'")
    expect(ps).not.toContain('gos3 restic env')
  })

  it('builds gos3 restic command lines that always name a source', () => {
    expect(gos3ResticCommand('bash', 'env', [['--bucket', 'photos'], ['--snapshots'], ['--shell', 'bash']], { server: 'https://s3.example.com' }))
      .toBe('gos3 restic env --bucket photos --snapshots --shell bash --server https://s3.example.com')
    expect(gos3ResticCommand('bash', 'recover', [['--bucket', 'my photos'], ['--key', "it's"]], { config: '/etc/gos3/gos3.yaml' }))
      .toBe("gos3 restic recover --bucket 'my photos' --key 'it'\\''s' --config /etc/gos3/gos3.yaml")
    expect(gos3ResticCommand('powershell', 'objects', [['--bucket', 'b']], {}))
      .toBe("gos3 restic objects --bucket 'b' --server 'SERVER_URL'")
    expect(gos3ResticCommand('bash', 'env', [['--bucket', 'b']], { server: '' })).toMatch(/--server SERVER_URL$/)
  })

  it('masks the password and secret variables for display', () => {
    const creds = { backing: 'default', purpose: 'archive', repository: '/r', password: 'pw', env: { AWS_ACCESS_KEY_ID: 'AKIA', AWS_SECRET_ACCESS_KEY: 'sk', AWS_SESSION_TOKEN: 'tok' }, options: null }
    const masked = maskCredentials(creds)
    expect(masked).toMatchObject({ password: MASK, env: { AWS_ACCESS_KEY_ID: 'AKIA', AWS_SECRET_ACCESS_KEY: MASK, AWS_SESSION_TOKEN: MASK } })
    expect(creds.password).toBe('pw')
    const shown = resticSnippet(snippetFromCredentials('bash', 'photos', masked, TAGS))
    expect(shown).not.toMatch(/\bpw\b|\bsk\b|tok/)
    expect(shown).toContain('AKIA')
    expect(isSecretEnv('RESTIC_PASSWORD')).toBe(true)
    expect(isSecretEnv('AWS_ACCESS_KEY_ID')).toBe(false)
  })

  it('folds line breaks in comments so a value cannot start a command', () => {
    const s = resticSnippet(snippetFromInfo('bash', 'b\nrm -rf /', 'archive', { ...info, envNames: [] }, TAGS))
    expect(s.split('\n').filter(l => l.startsWith('rm'))).toEqual([])
  })
})

describe('snapshot schedule form', () => {
  it('starts daily at 03:00 with the standard retention; prune off while archiving', () => {
    const f = defaultSnapshotForm()
    // The server chooses the repository of a new schedule.
    expect(f).toMatchObject({ enabled: true, interval: '24h', at: '03:00', targetKind: 'auto', pruneEnabled: true, pruneInterval: '168h' })
    expect(snapshotConfigFrom(f)).not.toHaveProperty('target')
    expect(retentionPreset(f.retention)).toBe('standard')
    expect(defaultSnapshotForm({ archiveOn: true }).pruneEnabled).toBe(false)
    expect(defaultSnapshotForm({ targetKind: 'backing', backing: 'cold' })).toMatchObject({ targetKind: 'backing', backing: 'cold' })
    expect(validateSnapshotForm(f)).toEqual({})
  })

  it('reads a stored config, normalizing durations, and writes it back', () => {
    const f = snapshotFormFrom(CONFIG)
    expect(f).toMatchObject({ interval: '24h', pruneInterval: '168h', targetKind: 'backing', backing: 'cold', repo: 'snap-1a2b' })
    // An empty prefix is left out.
    const { prefix: _prefix, ...rest } = CONFIG
    expect(snapshotConfigFrom(f)).toEqual({ ...rest, interval: '24h', prune: { enabled: false, interval: '168h' } })
    expect(snapshotFormDirty(f, CONFIG)).toBe(false)
    expect(snapshotFormDirty({ ...f, interval: '12h', at: '' }, CONFIG)).toBe(true)
    expect(snapshotFormDirty(f, null)).toBe(true)
    // Nanosecond durations (time.Duration) are understood too.
    expect(snapshotFormFrom({ ...CONFIG, interval: 3600e9 as unknown as string }).interval).toBe('1h')
  })

  it('writes Go durations, days included, and leaves out empty optional fields', () => {
    const cfg = snapshotConfigFrom({ ...defaultSnapshotForm(), interval: '7d', at: '', prefix: '', retention: { ...presetRetention('short'), keepWithin: '30d' }, pruneInterval: '14d' })
    expect(cfg).toEqual({
      enabled: true,
      interval: '168h',
      consistent: true,
      retention: { keepLast: 3, keepHourly: 0, keepDaily: 7, keepWeekly: 0, keepMonthly: 0, keepYearly: 0, keepWithin: '720h' },
      prune: { enabled: true, interval: '336h', default: true }
    })
    expect(snapshotConfigFrom({ ...defaultSnapshotForm(), targetKind: 'bucket' }).target).toEqual({ kind: 'bucket' })
    expect(snapshotConfigFrom({ ...defaultSnapshotForm(), targetKind: 'backing', backing: 'cold' }).target).toEqual({ kind: 'backing', backing: 'cold' })
    // A stored schedule keeps its kind; one without a target reads as automatic.
    expect(snapshotFormFrom({ ...CONFIG, target: { kind: 'bucket' } }).targetKind).toBe('bucket')
    expect(snapshotFormFrom({ ...CONFIG, target: undefined }).targetKind).toBe('auto')
    expect(describeTarget(CONFIG.target)).toBe('Repository snap-1a2b on cold')
    expect(describeTarget({ kind: 'bucket' })).toBe('The bucket’s own repository')
    expect(describeTarget(undefined)).toBe('Chosen by the server')
  })

  it('sends prune back as the server default unless the user changed enabled', () => {
    const stored: SnapshotConfig = { ...CONFIG, prune: { enabled: false, interval: '168h0m0s', default: true } }
    const f = snapshotFormFrom(stored)
    expect(f.pruneDefault).toBe(true)
    // Untouched (an interval change does not count): default stays.
    expect(snapshotConfigFrom(f).prune).toEqual({ enabled: false, interval: '168h', default: true })
    expect(snapshotConfigFrom({ ...f, pruneInterval: '336h' }).prune).toEqual({ enabled: false, interval: '336h', default: true })
    // Changing enabled makes it explicit (the switch clears pruneDefault).
    expect(snapshotConfigFrom({ ...f, pruneEnabled: true, pruneDefault: false }).prune).toEqual({ enabled: true, interval: '168h' })
    // An explicit stored choice stays explicit; a new schedule leaves it to the server.
    expect(snapshotConfigFrom(snapshotFormFrom(CONFIG)).prune).not.toHaveProperty('default')
    expect(snapshotConfigFrom(defaultSnapshotForm({ archiveOn: true })).prune).toEqual({ enabled: false, interval: '168h', default: true })
    expect(snapshotFormDirty(f, stored)).toBe(false)
  })

  it('validates interval, time of day, retention, target and prune', () => {
    const v = (over: Partial<ReturnType<typeof defaultSnapshotForm>>) => validateSnapshotForm({ ...defaultSnapshotForm(), ...over })
    expect(v({ interval: '30m' }).interval).toBe('Between 1h and 30d')
    expect(v({ interval: '31d' }).interval).toBe('Between 1h and 30d')
    expect(v({ interval: 'daily' }).interval).toBeTruthy()
    expect(v({ interval: '30d' }).interval).toBeUndefined()
    expect(v({ at: '3:00' }).at).toMatch(/HH:MM/)
    expect(v({ at: '24:00' }).at).toMatch(/HH:MM/)
    expect(v({ at: '23:59' }).at).toBeUndefined()
    expect(v({ interval: '6h', at: '03:00' }).at).toMatch(/24h or more/)
    expect(v({ interval: '6h', at: '' }).at).toBeUndefined()
    expect(v({ prefix: 'a\u0000' }).prefix).toBeTruthy()
    const none = { keepLast: 0, keepHourly: 0, keepDaily: 0, keepWeekly: 0, keepMonthly: 0, keepYearly: 0, keepWithin: '0s' }
    expect(v({ retention: none }).retention).toMatch(/at least one/)
    expect(v({ retention: { ...none, keepWithin: '30d' } }).retention).toBeUndefined()
    expect(v({ retention: { ...none, keepWithin: 'month' } }).keepWithin).toBeTruthy()
    expect(v({ retention: { ...none, keepLast: -1 } }).retention).toMatch(/whole numbers/)
    expect(v({ retention: { ...none, keepLast: 1.5 } }).retention).toMatch(/whole numbers/)
    expect(v({ targetKind: 'backing', backing: '' }).backing).toBeTruthy()
    expect(v({ pruneEnabled: true, pruneInterval: '10m' }).pruneInterval).toBe('At least 1h')
    expect(v({ pruneEnabled: false, pruneInterval: 'x' }).pruneInterval).toBeUndefined()
  })

  it('recognizes retention presets and describes retention and schedules', () => {
    for (const p of RETENTION_PRESETS) expect(retentionPreset(presetRetention(p.value))).toBe(p.value)
    expect(retentionPreset({ ...presetRetention('standard'), keepYearly: 1 })).toBe('custom')
    expect(retentionPreset({ ...presetRetention('standard'), keepWithin: '0h' })).toBe('standard')
    // Presets are copies: editing one does not change the preset.
    presetRetention('standard').keepLast = 99
    expect(presetRetention('standard').keepLast).toBe(7)
    expect(describeRetention(presetRetention('standard'))).toBe('7 last, 7 daily, 4 weekly, 12 monthly')
    expect(describeRetention({ ...presetRetention('short'), keepWithin: '720h' })).toBe('3 last, 7 daily, all within 720h')
    expect(describeSchedule(CONFIG)).toBe('Daily at 03:00 UTC')
    expect(describeSchedule({ ...CONFIG, interval: '6h', at: '' })).toBe('Every 6h')
    expect(describeSchedule({ ...CONFIG, interval: '168h' })).toBe('Weekly at 03:00 UTC')
    expect(describeSchedule({ ...CONFIG, enabled: false })).toBe('Paused')
  })

  it('recognizes partial snapshots by flag or tag', () => {
    expect(isPartialSnapshot({ partial: true })).toBe(true)
    expect(isPartialSnapshot({ tags: ['gos3-snapshot', 'gos3-partial'] })).toBe(true)
    expect(isPartialSnapshot({ tags: ['gos3-snapshot'] })).toBe(false)
    expect(isPartialSnapshot({})).toBe(false)
  })

  it('sorts snapshots newest first', () => {
    const s = (id: string, time: string) => ({ id, shortId: id, time })
    expect(sortSnapshots([s('a', '2026-10-01T00:00:00Z'), s('b', '2026-10-03T00:00:00Z'), s('c', '2026-10-02T00:00:00Z')]).map(x => x.id)).toEqual(['b', 'c', 'a'])
    expect(sortSnapshots(null)).toEqual([])
  })
})

describe('restore dialog', () => {
  it('suggests a free bucket name and builds the request', () => {
    expect(emptyRestoreForm('photos').bucket).toBe('photos-restored')
    expect(emptyRestoreForm('photos', ['photos-restored']).bucket).toBe('photos-restored-2')
    const f = { ...emptyRestoreForm('photos'), backing: 'cold', prefix: '2024/' }
    expect(restoreRequest(f)).toEqual({ target: { bucket: 'photos-restored', create: { backing: 'cold' } }, prefix: '2024/', configs: 'safe' })
    expect(restoreRequest({ ...f, target: 'existing', bucket: ' docs ', onExisting: 'merge', configs: 'none', prefix: '' })).toEqual({ target: { bucket: 'docs' }, onExisting: 'merge', configs: 'none' })
  })

  it('validates the target bucket and the prefix', () => {
    const f = emptyRestoreForm('photos')
    expect(validateRestoreForm(f, ['photos'])).toEqual({})
    expect(validateRestoreForm({ ...f, bucket: '' }, []).bucket).toBe('Enter a name for the new bucket')
    expect(validateRestoreForm({ ...f, bucket: 'photos' }, ['photos']).bucket).toMatch(/exists/)
    expect(validateRestoreForm({ ...f, bucket: 'Bad' }, []).bucket).toBeTruthy()
    expect(validateRestoreForm({ ...f, target: 'existing', bucket: 'photos' }, ['photos'])).toEqual({})
    expect(validateRestoreForm({ ...f, target: 'existing', bucket: 'nope' }, ['photos']).bucket).toBeTruthy()
    expect(validateRestoreForm({ ...f, target: 'existing', bucket: 'typed' }, [])).toEqual({})
    expect(validateRestoreForm({ ...f, prefix: 'x\u0007' }, []).prefix).toBeTruthy()
  })
})
