import { describe, expect, it } from 'vitest'
import type { Settings } from '~/types/admin'
import {
  dirtySections,
  formatList,
  formToSettings,
  isFormDirty,
  normalizeList,
  parseList,
  resolveSettingsSection,
  sectionOf,
  settingsSections,
  settingsToForm,
  validateBeaconGroup,
  validateDomain,
  validateExposeHeader,
  validateMediaTypePattern,
  validateOrigin,
  validateSeed,
  validateSettingsForm,
  validateSrvName
} from '~/components/admin/-settings-form'

function sample(): Settings {
  return {
    name: 'gos3-1',
    region: 'us-east-1',
    publicUrl: 'http://10.0.0.5:9000',
    domains: ['s3.local'],
    publicInfo: true,
    uiRedirect: true,
    cors: { allowedOrigins: ['*'], allowedHeaders: ['*'], exposeHeaders: ['ETag'], maxAgeSeconds: 3600 },
    discovery: {
      enabled: true,
      advertise: true,
      mdns: true,
      beacon: true,
      beaconGroup: '239.255.73.83:7373',
      beaconInterval: 10_000_000_000,
      seeds: null,
      dnsSrv: null,
      peerTtl: 60_000_000_000,
      allowScan: true,
      maxScanHosts: 1024
    },
    logging: { level: 'info', format: 'text', accessLog: true },
    lifecycle: { enabled: true, intervalSeconds: 3600 },
    metricsPublic: false
  }
}

describe('settings form conversions', () => {
  it('converts nanosecond durations to seconds and back', () => {
    const f = settingsToForm(sample())
    expect(f.discovery.beaconIntervalSeconds).toBe(10)
    expect(f.discovery.peerTtlSeconds).toBe(60)
    f.discovery.beaconIntervalSeconds = 2.5
    f.discovery.peerTtlSeconds = 90
    const s = formToSettings(f, sample())
    expect(s.discovery.beaconInterval).toBe(2_500_000_000)
    expect(s.discovery.peerTtl).toBe(90_000_000_000)
  })

  it('keeps millisecond precision for odd nanosecond values', () => {
    const s = sample()
    s.discovery.beaconInterval = 1_234_567_891
    expect(settingsToForm(s).discovery.beaconIntervalSeconds).toBe(1.235)
  })

  it('turns null lists into empty arrays and back into normalised lists', () => {
    const f = settingsToForm(sample())
    expect(f.discovery.seeds).toEqual([])
    expect(f.discovery.dnsSrv).toEqual([])
    f.discovery.seeds = ['  http://a:9000 ', '', 'http://a:9000', 'b:9000']
    const s = formToSettings(f)
    expect(s.discovery.seeds).toEqual(['http://a:9000', 'b:9000'])
    expect(s.discovery.dnsSrv).toEqual([])
  })

  it('round-trips a settings document unchanged', () => {
    const base = sample()
    const out = formToSettings(settingsToForm(base), base)
    expect(out).toEqual({ ...base, discovery: { ...base.discovery, seeds: [], dnsSrv: [] } })
  })

  it('preserves unknown fields from the server document', () => {
    const base = { ...sample(), futureField: 42, cors: { ...sample().cors, extra: 'x' } } as unknown as Settings
    const out = formToSettings(settingsToForm(base), base) as unknown as Record<string, unknown>
    expect(out.futureField).toBe(42)
    expect((out.cors as Record<string, unknown>).extra).toBe('x')
  })

  it('does not alias arrays of the source document', () => {
    const base = sample()
    const f = settingsToForm(base)
    f.domains.push('other.local')
    expect(base.domains).toEqual(['s3.local'])
  })

  it('strips a trailing slash from the public URL', () => {
    const f = settingsToForm(sample())
    f.publicUrl = 'https://s3.example.com/'
    expect(formToSettings(f).publicUrl).toBe('https://s3.example.com')
  })
})

describe('compression settings', () => {
  // A server with compression (sample() is a server without it).
  function withCompression(): Settings {
    return { ...sample(), compression: { mode: 'enforced', level: 6, minSize: 4096, skipContentTypes: ['image/jpeg', 'video/*', 'application/vnd.oasis.opendocument.*'] } }
  }

  it('round-trips the compression section', () => {
    const base = withCompression()
    const f = settingsToForm(base)
    expect(f.compression).toEqual({ mode: 'enforced', level: 6, minSize: 4096, skipContentTypes: ['image/jpeg', 'video/*', 'application/vnd.oasis.opendocument.*'] })
    expect(formToSettings(f, base)).toEqual({ ...base, discovery: { ...base.discovery, seeds: [], dnsSrv: [] } })
    expect(validateSettingsForm(f)).toEqual({})
  })

  it('saves edited fields with normalised lists', () => {
    const f = settingsToForm(withCompression())
    f.compression!.mode = 'disabled'
    f.compression!.level = 9
    f.compression!.minSize = 0
    f.compression!.skipContentTypes = [' audio/* ', '', 'audio/*', 'font/woff2']
    expect(formToSettings(f, withCompression()).compression).toEqual({ mode: 'disabled', level: 9, minSize: 0, skipContentTypes: ['audio/*', 'font/woff2'] })
  })

  it('treats a null skip list as empty and keeps unknown compression fields', () => {
    const base = { ...sample(), compression: { mode: 'optional', level: 1, minSize: 4096, skipContentTypes: null, future: true } } as unknown as Settings
    const f = settingsToForm(base)
    expect(f.compression!.skipContentTypes).toEqual([])
    const out = formToSettings(f, base).compression as unknown as Record<string, unknown>
    expect(out.skipContentTypes).toEqual([])
    expect(out.future).toBe(true)
  })

  it('sends no compression section to a server that has none', () => {
    const base = sample()
    const f = settingsToForm(base)
    expect(f.compression).toBeNull()
    const out = formToSettings(f, base)
    expect('compression' in out).toBe(false)
    expect(out).toEqual({ ...base, discovery: { ...base.discovery, seeds: [], dnsSrv: [] } })
    expect(validateSettingsForm(f)).toEqual({})
    expect(formToSettings(f)).not.toHaveProperty('compression')
  })

  it('detects compression changes as dirty', () => {
    const orig = settingsToForm(withCompression())
    const cur = settingsToForm(withCompression())
    cur.compression!.skipContentTypes = [...cur.compression!.skipContentTypes, ' video/* ']
    expect(isFormDirty(cur, orig)).toBe(false)
    cur.compression!.level = 1
    expect(isFormDirty(cur, orig)).toBe(true)
  })

  it('validates mode, level, minimum size and media type patterns like the server', () => {
    const f = settingsToForm(withCompression())
    f.compression!.mode = 'sometimes'
    f.compression!.level = 10
    f.compression!.minSize = -1
    f.compression!.skipContentTypes = ['image/jpeg', 'text/plain; charset=utf-8']
    expect(validateSettingsForm(f)).toEqual({
      'compression.mode': 'Unknown compression mode',
      'compression.level': 'Must be a whole number between 1 and 9',
      'compression.minSize': expect.stringMatching(/between 0 and/),
      'compression.skipContentTypes': expect.stringMatching(/text\/plain; charset=utf-8/)
    })
    f.compression!.mode = 'optional'
    f.compression!.level = 1.5
    f.compression!.minSize = 5 * 1024 ** 4 + 1
    f.compression!.skipContentTypes = Array.from({ length: 257 }, (_, i) => `x/t${i}`)
    expect(Object.keys(validateSettingsForm(f)).sort()).toEqual(['compression.level', 'compression.minSize', 'compression.skipContentTypes'])
    f.compression!.level = 1
    f.compression!.minSize = 5 * 1024 ** 4
    f.compression!.skipContentTypes = ['video/*']
    expect(validateSettingsForm(f)).toEqual({})
    expect(sectionOf('compression.level')).toBe('compression')
  })

  it('accepts the media type pattern forms', () => {
    for (const t of ['image/jpeg', 'video/*', 'application/vnd.oasis.opendocument.*', 'application/epub+zip', 'Image/JPEG', 'application/x-7z-compressed']) {
      expect(validateMediaTypePattern(t)).toBeUndefined()
    }
    for (const t of ['*', 'image', '*/*', 'image/', '/jpeg', 'image/jp*eg', 'image/**', 'text/plain;charset=utf-8', 'image /jpeg']) {
      expect(validateMediaTypePattern(t)).toBeDefined()
    }
  })
})

describe('list editors', () => {
  it('normalizeList trims, drops empties and dedupes', () => {
    expect(normalizeList([' a ', 'b', '', 'a', null, undefined, 'c '])).toEqual(['a', 'b', 'c'])
    expect(normalizeList(null)).toEqual([])
  })

  it('parseList splits on newlines, commas and spaces', () => {
    expect(parseList('a, b\nc  d,,\n a')).toEqual(['a', 'b', 'c', 'd'])
    expect(parseList('')).toEqual([])
  })

  it('formatList joins one item per line', () => {
    expect(formatList(['a', 'b'])).toBe('a\nb')
    expect(formatList(null)).toBe('')
  })
})

describe('dirty tracking', () => {
  it('ignores list whitespace / duplicates but detects real changes', () => {
    const orig = settingsToForm(sample())
    const cur = settingsToForm(sample())
    expect(isFormDirty(cur, orig)).toBe(false)
    cur.domains = ['s3.local', ' s3.local ', '']
    expect(isFormDirty(cur, orig)).toBe(false)
    cur.discovery.beaconIntervalSeconds = 11
    expect(isFormDirty(cur, orig)).toBe(true)
    expect(isFormDirty(cur, null)).toBe(false)
  })
})

describe('settings validation', () => {
  it('accepts the defaults', () => {
    expect(validateSettingsForm(settingsToForm(sample()))).toEqual({})
  })

  it('flags invalid fields by path', () => {
    const f = settingsToForm(sample())
    f.name = ' '
    f.region = 'US East'
    f.publicUrl = 'ftp://x'
    f.domains = ['http://s3.local']
    f.cors.allowedOrigins = ['example.com']
    f.cors.maxAgeSeconds = -1
    f.discovery.beaconGroup = '10.0.0.1:7373'
    f.discovery.beaconIntervalSeconds = 0
    f.discovery.maxScanHosts = 0
    f.discovery.seeds = ['not a url']
    f.logging.level = 'trace'
    f.lifecycle.intervalSeconds = 1.5
    const e = validateSettingsForm(f)
    expect(Object.keys(e).sort()).toEqual([
      'cors.allowedOrigins',
      'cors.maxAgeSeconds',
      'discovery.beaconGroup',
      'discovery.beaconIntervalSeconds',
      'discovery.maxScanHosts',
      'discovery.seeds',
      'domains',
      'lifecycle.intervalSeconds',
      'logging.level',
      'name',
      'publicUrl',
      'region'
    ])
  })

  it('requires a peer TTL of at least twice the beacon interval when beaconing', () => {
    const f = settingsToForm(sample())
    f.discovery.peerTtlSeconds = 15
    expect(validateSettingsForm(f)['discovery.peerTtlSeconds']).toMatch(/twice/)
    f.discovery.beacon = false
    expect(validateSettingsForm(f)['discovery.peerTtlSeconds']).toBeUndefined()
  })

  it('validates individual list items', () => {
    expect(validateDomain('s3.local')).toBeUndefined()
    // The server matches Host without the port, so domains carry none.
    expect(validateDomain('s3.local:9000')).toBeDefined()
    expect(validateDomain('-bad.local')).toBeDefined()
    expect(validateOrigin('*')).toBeUndefined()
    expect(validateOrigin('https://*.example.com')).toBeUndefined()
    expect(validateOrigin('https://*.*.example.com')).toBeDefined()
    expect(validateBeaconGroup('239.255.73.83:7373')).toBeUndefined()
    expect(validateBeaconGroup('239.255.73.83')).toBeDefined()
    expect(validateBeaconGroup('240.0.0.1:7373')).toBeDefined()
    expect(validateSeed('http://10.0.0.5:9000')).toBeUndefined()
    expect(validateSeed('nas.local:9000')).toBeUndefined()
    expect(validateSeed('nas.local')).toBeUndefined()
    expect(validateSeed('http://')).toBeDefined()
    // Like the server: base URLs only (no path, query or credentials).
    expect(validateSeed('http://nas:9000/')).toBeUndefined()
    expect(validateSeed('http://nas:9000/sub')).toBeDefined()
    expect(validateSeed('http://u:p@nas:9000')).toBeDefined()
    expect(validateSrvName('example.com')).toBeUndefined()
    expect(validateSrvName('example.com.')).toBeUndefined()
    expect(validateSrvName('exa mple.com')).toBeDefined()
    expect(validateExposeHeader('*')).toBeUndefined()
    expect(validateExposeHeader('ETag')).toBeUndefined()
    expect(validateExposeHeader('x-amz-*')).toBeDefined()
  })

  it('accepts the settings a real gos3 returns by default (GET then PUT round trip)', () => {
    // Shape of GET /config from a gos3 started with defaults: exposeHeaders
    // starts with "*" (all headers readable cross-origin).
    const s: Settings = {
      ...sample(),
      domains: [],
      cors: {
        allowedOrigins: ['http://localhost:3000'],
        allowedHeaders: ['*'],
        exposeHeaders: ['*', 'ETag', 'Content-Length', 'x-amz-version-id', 'x-amz-delete-marker', 'x-amz-checksum-crc64nvme', 'x-amz-object-lock-mode'],
        maxAgeSeconds: 3600
      },
      discovery: { ...sample().discovery, mdns: false, beacon: false, seeds: ['http://127.0.0.1:19401'], dnsSrv: [] }
    }
    expect(validateSettingsForm(settingsToForm(s))).toEqual({})
  })

  it('enforces the server limits', () => {
    const f = settingsToForm(sample())
    f.discovery.beaconIntervalSeconds = 3601
    f.discovery.maxScanHosts = 65537
    f.discovery.peerTtlSeconds = 8 * 24 * 3600
    f.cors.maxAgeSeconds = 604801
    f.lifecycle.intervalSeconds = 59
    f.region = 'us-east-'
    f.discovery.dnsSrv = Array.from({ length: 65 }, (_, i) => `d${i}.example.com`)
    expect(Object.keys(validateSettingsForm(f)).sort()).toEqual([
      'cors.maxAgeSeconds',
      'discovery.beaconIntervalSeconds',
      'discovery.dnsSrv',
      'discovery.maxScanHosts',
      'discovery.peerTtlSeconds',
      'lifecycle.intervalSeconds',
      'region'
    ])
    f.discovery.beaconIntervalSeconds = 3600
    f.discovery.maxScanHosts = 65536
    f.discovery.peerTtlSeconds = 7 * 24 * 3600
    f.cors.maxAgeSeconds = 604800
    f.lifecycle.intervalSeconds = 60
    f.region = 'us-east-1'
    f.discovery.dnsSrv = ['example.com']
    expect(validateSettingsForm(f)).toEqual({})
  })

  it('maps field paths to sections', () => {
    expect(sectionOf('name')).toBe('general')
    expect(sectionOf('cors.maxAgeSeconds')).toBe('cors')
    expect(sectionOf('discovery.seeds')).toBe('discovery')
  })
})

describe('settings sections', () => {
  it('offers compression only when the server reported it', () => {
    expect(settingsSections(settingsToForm(sample())).map(s => s.value)).toEqual(['general', 'cors', 'discovery', 'logging', 'lifecycle'])
    const withCompression = settingsToForm({ ...sample(), compression: { mode: 'optional', level: 1, minSize: 0, skipContentTypes: [] } })
    expect(settingsSections(withCompression).map(s => s.value)).toContain('compression')
    expect(settingsSections(null).map(s => s.value)).not.toContain('compression')
  })

  it('resolves ?tab= to a known section (default general)', () => {
    const sections = settingsSections(settingsToForm(sample()))
    expect(resolveSettingsSection('discovery', sections)).toBe('discovery')
    expect(resolveSettingsSection(['logging', 'cors'], sections)).toBe('logging')
    expect(resolveSettingsSection('compression', sections)).toBe('general')
    expect(resolveSettingsSection(undefined, sections)).toBe('general')
    expect(resolveSettingsSection('bogus')).toBe('general')
  })

  it('reports the sections with unsaved changes', () => {
    const original = settingsToForm(sample())
    const f = settingsToForm(sample())
    expect(dirtySections(f, original).size).toBe(0)
    expect(dirtySections(f, null).size).toBe(0)
    f.name = 'renamed'
    f.discovery.beaconIntervalSeconds = 5
    expect([...dirtySections(f, original)].sort()).toEqual(['discovery', 'general'])
    // Normalised like the saved document: whitespace and duplicates are no change.
    const g = settingsToForm(sample())
    g.cors.allowedOrigins = [' * ', '*']
    g.region = ' us-east-1 '
    expect(dirtySections(g, original).size).toBe(0)
  })

  it('maps every section field path to its section', () => {
    expect(sectionOf('logging.level')).toBe('logging')
    expect(sectionOf('lifecycle.intervalSeconds')).toBe('lifecycle')
    expect(sectionOf('publicUrl')).toBe('general')
  })
})
