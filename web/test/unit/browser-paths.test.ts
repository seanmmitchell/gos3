import { describe, expect, it } from 'vitest'
import {
  attachmentDisposition,
  browserCrumbs,
  browserObjectRoute,
  browserRoute,
  encodeKeyPath,
  entryId,
  entryVersionParam,
  filterBrowserEntries,
  folderKey,
  guessContentType,
  mergeBrowserEntries,
  normalizeRelativePath,
  parseBrowserPath,
  rebaseKey,
  relativeName,
  sortBrowserEntries,
  toBrowserEntries,
  toBrowserVersionEntries,
  uploadKey,
  validateFolderName
} from '~/composables/useBrowserPaths'
import type { BrowserEntry } from '~/composables/useBrowserPaths'
import { appRoutes } from '~/utils/routes'

describe('browser routes', () => {
  it('builds folder routes with per-segment encoding and a trailing slash', () => {
    expect(browserRoute('srv', 'photos')).toBe('/servers/srv/buckets/photos/objects/')
    expect(browserRoute('srv', 'photos', 'a/b/')).toBe('/servers/srv/buckets/photos/objects/a/b/')
    expect(browserRoute('srv', 'photos', 'a/b')).toBe('/servers/srv/buckets/photos/objects/a/b/')
    expect(browserRoute('my srv', 'b', 'x y/100%/#1?/')).toBe('/servers/my%20srv/buckets/b/objects/x%20y/100%25/%231%3F/')
    expect(encodeKeyPath('a b/c+d/')).toBe('a%20b/c%2Bd/')
  })

  it('object routes match appRoutes.object', () => {
    const key = 'docs/ä ö/file #1.txt'
    expect(browserObjectRoute('s', 'b', key)).toBe(appRoutes.object('s', 'b', key))
    expect(browserObjectRoute('s', 'b', key, 'v1')).toBe(appRoutes.object('s', 'b', key, 'v1'))
  })

  it('parses route paths: trailing slash = folder, else object', () => {
    expect(parseBrowserPath('/servers/s/buckets/b/objects')).toEqual({ kind: 'folder', prefix: '' })
    expect(parseBrowserPath('/servers/s/buckets/b/objects/')).toEqual({ kind: 'folder', prefix: '' })
    expect(parseBrowserPath('/servers/s/buckets/b/objects/a/b%20c/')).toEqual({ kind: 'folder', prefix: 'a/b c/' })
    expect(parseBrowserPath('/servers/s/buckets/b/objects/a/f.txt')).toEqual({ kind: 'object', key: 'a/f.txt', prefix: 'a/' })
    expect(parseBrowserPath('/servers/s/buckets/b/objects/top.txt')).toEqual({ kind: 'object', key: 'top.txt', prefix: '' })
    // Malformed escapes are kept verbatim instead of throwing.
    expect(parseBrowserPath('/servers/s/buckets/b/objects/100%/')).toEqual({ kind: 'folder', prefix: '100%/' })
  })

  it('round-trips arbitrary keys through route building and parsing', () => {
    const keys = ['a/b/c.txt', 'spaces in/name.md', 'unicode/日本語/ファイル', 'weird/%20/#?&=+', 'emoji/😀.png']
    for (const key of keys) {
      expect(parseBrowserPath(browserObjectRoute('s', 'b', key))).toEqual({ kind: 'object', key, prefix: key.slice(0, key.lastIndexOf('/') + 1) })
      const folder = `${key}/`
      expect(parseBrowserPath(browserRoute('s', 'b', folder))).toEqual({ kind: 'folder', prefix: folder })
    }
  })

  it('builds breadcrumbs starting at the bucket', () => {
    expect(browserCrumbs('s', 'b', '')).toEqual([{ label: 'b', prefix: '', to: '/servers/s/buckets/b/objects/' }])
    expect(browserCrumbs('s', 'b', 'x/y z/')).toEqual([
      { label: 'b', prefix: '', to: '/servers/s/buckets/b/objects/' },
      { label: 'x', prefix: 'x/', to: '/servers/s/buckets/b/objects/x/' },
      { label: 'y z', prefix: 'x/y z/', to: '/servers/s/buckets/b/objects/x/y%20z/' }
    ])
  })
})

describe('listing model', () => {
  it('builds folder and object entries, hiding the folder placeholder itself', () => {
    const rows = toBrowserEntries(
      [{ prefix: 'docs/sub/' }],
      [
        { key: 'docs/', size: 0 },
        { key: 'docs/a.txt', size: 5, lastModified: '2026-01-01T00:00:00Z', storageClass: 'STANDARD', etag: 'e1' }
      ],
      'docs/'
    )
    expect(rows).toEqual([
      { type: 'folder', id: 'docs/sub/', key: 'docs/sub/', name: 'sub' },
      { type: 'object', id: 'docs/a.txt', key: 'docs/a.txt', name: 'a.txt', size: 5, lastModified: '2026-01-01T00:00:00Z', storageClass: 'STANDARD', etag: 'e1' }
    ])
  })

  it('builds version entries with unique ids per version', () => {
    const rows = toBrowserVersionEntries([], [
      { key: 'a.txt', versionId: 'v2', isLatest: true, isDeleteMarker: true, size: 0 },
      { key: 'a.txt', versionId: 'v1', isLatest: false, isDeleteMarker: false, size: 3 }
    ], '')
    expect(rows.map(r => r.id)).toEqual([entryId('a.txt', 'v2'), entryId('a.txt', 'v1')])
    expect(new Set(rows.map(r => r.id)).size).toBe(2)
    expect(rows[0]).toMatchObject({ isDeleteMarker: true, isLatest: true, versionId: 'v2' })
  })

  it('relative names strip the listed prefix', () => {
    expect(relativeName('a/b/c.txt', 'a/')).toBe('b/c.txt')
    expect(relativeName('a/b/', 'a/')).toBe('b')
    expect(relativeName('x.txt', '')).toBe('x.txt')
  })

  it('merges pages without duplicates', () => {
    const a: BrowserEntry[] = [{ type: 'folder', id: 'f/', key: 'f/', name: 'f' }]
    const b: BrowserEntry[] = [{ type: 'folder', id: 'f/', key: 'f/', name: 'f' }, { type: 'folder', id: 'g/', key: 'g/', name: 'g' }]
    expect(mergeBrowserEntries(a, b).map(e => e.id)).toEqual(['f/', 'g/'])
  })

  const sample: BrowserEntry[] = [
    { type: 'object', id: 'b.txt', key: 'b.txt', name: 'b.txt', size: 10, lastModified: '2026-01-02T00:00:00Z' },
    { type: 'folder', id: 'z/', key: 'z/', name: 'z' },
    { type: 'object', id: 'a10.txt', key: 'a10.txt', name: 'a10.txt', size: 30, lastModified: '2026-01-01T00:00:00Z' },
    { type: 'folder', id: 'm/', key: 'm/', name: 'm' },
    { type: 'object', id: 'a2.txt', key: 'a2.txt', name: 'a2.txt', size: 20, lastModified: '2026-01-03T00:00:00Z' }
  ]

  it('sorts folders first, then by the chosen column (natural name order)', () => {
    expect(sortBrowserEntries(sample, { by: 'name', desc: false }).map(e => e.name)).toEqual(['m', 'z', 'a2.txt', 'a10.txt', 'b.txt'])
    expect(sortBrowserEntries(sample, { by: 'name', desc: true }).map(e => e.name)).toEqual(['z', 'm', 'b.txt', 'a10.txt', 'a2.txt'])
    expect(sortBrowserEntries(sample, { by: 'size', desc: true }).map(e => e.name)).toEqual(['m', 'z', 'a10.txt', 'a2.txt', 'b.txt'])
    expect(sortBrowserEntries(sample, { by: 'modified', desc: false }).map(e => e.name)).toEqual(['m', 'z', 'a10.txt', 'b.txt', 'a2.txt'])
    // Does not mutate the input.
    expect(sample[0]!.name).toBe('b.txt')
  })

  it('keeps versions of one key newest first', () => {
    const versions: BrowserEntry[] = [
      { type: 'object', id: 'k\u0000v1', key: 'k', name: 'k', size: 1, versionId: 'v1', lastModified: '2026-01-01T00:00:00Z' },
      { type: 'object', id: 'k\u0000v2', key: 'k', name: 'k', size: 1, versionId: 'v2', lastModified: '2026-02-01T00:00:00Z' }
    ]
    expect(sortBrowserEntries(versions, { by: 'name', desc: false }).map(e => e.id)).toEqual(['k\u0000v2', 'k\u0000v1'])
    expect(sortBrowserEntries(versions, { by: 'name', desc: true }).map(e => e.id)).toEqual(['k\u0000v2', 'k\u0000v1'])
  })

  it('filters by name case-insensitively', () => {
    expect(filterBrowserEntries(sample, 'A1').map(e => e.name)).toEqual(['a10.txt'])
    expect(filterBrowserEntries(sample, '  ')).toBe(sample)
  })
})

describe('keys for new objects', () => {
  it('normalises relative upload paths', () => {
    expect(normalizeRelativePath('./a\\b//c.txt')).toBe('a/b/c.txt')
    expect(uploadKey('docs/', 'photos/2024/a.jpg')).toBe('docs/photos/2024/a.jpg')
    expect(uploadKey('', '/x.txt')).toBe('x.txt')
  })

  it('validates folder names and builds folder keys', () => {
    expect(validateFolderName('')).toBeDefined()
    expect(validateFolderName('a//b')).toBeDefined()
    expect(validateFolderName('/a')).toBeDefined()
    expect(validateFolderName('a/../b')).toBeDefined()
    expect(validateFolderName('reports/2026')).toBeUndefined()
    expect(validateFolderName('new folder/')).toBeUndefined()
    expect(folderKey('docs/', 'reports/2026/')).toBe('docs/reports/2026/')
    expect(folderKey('', ' x ')).toBe('x/')
  })

  it('rebases keys for copy / move', () => {
    expect(rebaseKey('a/b/c.txt', 'a/', 'x/')).toBe('x/b/c.txt')
    expect(rebaseKey('a/b/c.txt', 'a/', '')).toBe('b/c.txt')
    expect(rebaseKey('c.txt', '', 'dst/')).toBe('dst/c.txt')
  })

  it('builds RFC 5987 attachment dispositions', () => {
    expect(attachmentDisposition('report.pdf')).toBe('attachment; filename="report.pdf"; filename*=UTF-8\'\'report.pdf')
    expect(attachmentDisposition('ä "q".txt')).toBe('attachment; filename="_ _q_.txt"; filename*=UTF-8\'\'%C3%A4%20%22q%22.txt')
  })

  it('guesses content types', () => {
    expect(guessContentType('a/readme.MD')).toBe('text/markdown')
    expect(guessContentType('x.json', 'application/x-custom')).toBe('application/x-custom')
    expect(guessContentType('noext')).toBeUndefined()
  })
})

describe('entryVersionParam', () => {
  it('omits the version of the latest object but keeps old versions and delete markers', () => {
    expect(entryVersionParam({})).toBeUndefined()
    expect(entryVersionParam({ versionId: 'v2', isLatest: true })).toBeUndefined()
    expect(entryVersionParam({ versionId: 'v1', isLatest: false })).toBe('v1')
    // A latest delete marker must be addressed by its version: the plain key is 404.
    expect(entryVersionParam({ versionId: 'dm', isLatest: true, isDeleteMarker: true })).toBe('dm')
    expect(entryVersionParam({ versionId: 'dm0', isLatest: false, isDeleteMarker: true })).toBe('dm0')
  })
})
