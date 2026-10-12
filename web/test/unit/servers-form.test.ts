// Server form model / validation helpers (app/components/servers/-form.ts, -default.ts).
import { beforeEach, describe, expect, it } from 'vitest'
import type { Connection } from '~/types/connection'
import {
  connectionColor,
  copyName,
  duplicateInput,
  emptyServerForm,
  formFromConnection,
  formFromInput,
  formToInput,
  isDefinitionLocked,
  isMixedContent,
  peerEndpoint,
  prefillFromPeer,
  resolveKind,
  safeRedirect,
  validateEndpoint,
  validateRegion,
  validateServerForm
} from '~/components/servers/-form'
import { createPinia, setActivePinia } from 'pinia'
import { defaultFirst, useDefaultServer } from '~/components/servers/-default'
import { LOCAL_STORAGE_KEY, useConnectionsStore } from '~/stores/connections'

const CONN: Connection = {
  id: 'abc',
  name: 'NAS',
  endpoint: 'http://10.0.0.5:9000',
  region: 'us-east-1',
  accessKeyId: 'AK',
  secretAccessKey: 'SK',
  kind: 'gos3',
  forcePathStyle: true,
  remember: false,
  source: 'user',
  serverId: 'b3f1c2d4e5f60718',
  color: '#14b8a6'
}

describe('endpoint & region validation', () => {
  it('accepts URLs and bare hosts', () => {
    expect(validateEndpoint('http://10.0.0.5:9000')).toBeUndefined()
    expect(validateEndpoint('https://s3.eu-west-1.amazonaws.com/')).toBeUndefined()
    expect(validateEndpoint('nas.local:9000')).toBeUndefined()
  })

  it('rejects bad URLs with helpful messages', () => {
    expect(validateEndpoint('')).toMatch(/Enter the server URL/)
    expect(validateEndpoint('ftp://x')).toMatch(/http/)
    expect(validateEndpoint('http://a b')).toMatch(/spaces/)
    expect(validateEndpoint('http://user:pw@host')).toMatch(/credentials/)
    expect(validateEndpoint('http://host/?x=1')).toMatch(/query/)
    expect(validateEndpoint('http://')).toBeDefined()
  })

  it('validates regions', () => {
    expect(validateRegion('')).toBeUndefined()
    expect(validateRegion('eu-west-1')).toBeUndefined()
    expect(validateRegion('eu west')).toMatch(/letters/)
  })

  it('validates the whole form', () => {
    const m = { ...emptyServerForm(), endpoint: 'http://h:9000' }
    expect(validateServerForm(m)).toEqual({})
    expect(validateServerForm({ ...m, accessKeyId: 'AK' })).toHaveProperty('secretAccessKey')
    expect(validateServerForm({ ...m, secretAccessKey: 'SK' })).toHaveProperty('accessKeyId')
    expect(validateServerForm(m, { requireCredentials: true })).toEqual({ accessKeyId: 'Enter the access key', secretAccessKey: 'Enter the secret key' })
    expect(validateServerForm({ ...m, accessKeyId: 'A K', secretAccessKey: 's' })).toHaveProperty('accessKeyId')
  })
})

describe('form ↔ connection', () => {
  it('round-trips a connection', () => {
    const m = formFromConnection(CONN)
    expect(m.kind).toBe('gos3')
    const input = formToInput({ ...m, endpoint: 'http://10.0.0.5:9000/', region: ' ' })
    expect(input).toMatchObject({ endpoint: 'http://10.0.0.5:9000', region: 'us-east-1', kind: 'gos3', accessKeyId: 'AK', serverId: CONN.serverId, color: '#14b8a6' })
  })

  it('resolves the auto kind', () => {
    expect(resolveKind('auto', 'gos3')).toBe('gos3')
    expect(resolveKind('auto')).toBe('s3')
    expect(resolveKind('s3', 'gos3')).toBe('s3')
    const input = formToInput({ ...formFromInput({ endpoint: 'h:1' }), name: '  ' }, 'gos3')
    expect(input.kind).toBe('gos3')
    expect(input.name).toBeUndefined()
  })

  it('knows locked definitions', () => {
    expect(isDefinitionLocked({ source: 'preset' })).toBe(true)
    expect(isDefinitionLocked({ source: 'embedded' })).toBe(true)
    expect(isDefinitionLocked({ source: 'user' })).toBe(false)
    expect(isDefinitionLocked(undefined)).toBe(false)
  })

  it('duplicates with unique names', () => {
    expect(copyName('NAS', [])).toBe('NAS (copy)')
    expect(copyName('NAS', ['NAS (copy)'])).toBe('NAS (copy 2)')
    expect(copyName('NAS (copy)', ['NAS (copy)', 'NAS (copy 2)'])).toBe('NAS (copy 3)')
    const d = duplicateInput({ ...CONN, source: 'preset' }, ['NAS'])
    expect(d).toMatchObject({ name: 'NAS (copy)', source: 'user', accessKeyId: 'AK' })
    expect(d.id).toBeUndefined()
  })
})

describe('misc helpers', () => {
  it('maps colors', () => {
    expect(connectionColor('#abc')).toBe('#abc')
    expect(connectionColor('primary')).toBe('var(--ui-primary)')
    expect(connectionColor('url(evil)')).toBeUndefined()
    expect(connectionColor(undefined)).toBeUndefined()
  })

  it('detects mixed content', () => {
    expect(isMixedContent('http://10.0.0.5:9000', 'https:')).toBe(true)
    expect(isMixedContent('http://localhost:9000', 'https:')).toBe(false)
    expect(isMixedContent('https://x', 'https:')).toBe(false)
    expect(isMixedContent('http://x', 'http:')).toBe(false)
  })

  it('prefills from discovered peers', () => {
    const peer = { id: 'p1', name: 'other', region: 'eu', endpoints: { s3: '', admin: '', all: ['http://10.0.0.9:9000/'] }, addresses: ['http://10.0.0.8:9000'] }
    expect(peerEndpoint(peer)).toBe('http://10.0.0.9:9000')
    expect(prefillFromPeer(peer, CONN)).toMatchObject({ endpoint: 'http://10.0.0.9:9000', name: 'other', region: 'eu', serverId: 'p1', kind: 'gos3', source: 'discovered', accessKeyId: 'AK', secretAccessKey: 'SK' })
    expect(prefillFromPeer({ ...peer, endpoints: { s3: '', admin: '' }, addresses: [] })).toBeUndefined()
  })

  it('accepts only internal redirects', () => {
    expect(safeRedirect('/servers/x/buckets?prefix=a')).toBe('/servers/x/buckets?prefix=a')
    expect(safeRedirect(['/servers'])).toBe('/servers')
    expect(safeRedirect('//evil.com')).toBeUndefined()
    expect(safeRedirect('/\\evil.com')).toBeUndefined()
    expect(safeRedirect('https://evil.com')).toBeUndefined()
    expect(safeRedirect('/connect?server=x')).toBeUndefined()
    expect(safeRedirect(undefined)).toBeUndefined()
  })
})

describe('default server', () => {
  beforeEach(() => {
    localStorage.clear()
    setActivePinia(createPinia())
  })

  it('reads and writes the preference through the connections store', () => {
    const store = useConnectionsStore()
    const a = store.add({ name: 'A', endpoint: 'http://a:9000' })
    const { defaultId, setDefault } = useDefaultServer()
    expect(defaultId.value).toBeNull()
    setDefault(a.id)
    expect(defaultId.value).toBe(a.id)
    expect(store.defaultId).toBe(a.id)
    expect(JSON.parse(localStorage.getItem(LOCAL_STORAGE_KEY) ?? '{}').defaultId).toBe(a.id)
    setDefault(null)
    expect(defaultId.value).toBeNull()
  })

  it('sorts the default first', () => {
    const list = [{ id: 'a' }, { id: 'b' }, { id: 'c' }]
    expect(defaultFirst(list, 'c').map(x => x.id)).toEqual(['c', 'a', 'b'])
    expect(defaultFirst(list, 'zzz').map(x => x.id)).toEqual(['a', 'b', 'c'])
    expect(defaultFirst(list, null)).toEqual(list)
  })
})
