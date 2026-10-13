// Server form model / validation helpers (app/components/servers/-form.ts, -default.ts).
import { beforeEach, describe, expect, it } from 'vitest'
import type { Connection } from '~/types/connection'
import {
  connectionColor,
  copyName,
  credentialReuseHost,
  duplicateInput,
  emptyServerForm,
  formFromConnection,
  formFromInput,
  formToInput,
  isDefinitionLocked,
  isMixedContent,
  linkPrefillCheck,
  peerAdvertisedEndpoint,
  peerEndpoint,
  prefillFromPeer,
  resolveKind,
  reusableConnection,
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

  it('prefills from discovered peers with the observed address, never the self-declared endpoint', () => {
    const peer = { id: 'p1', name: 'other', region: 'eu', endpoints: { s3: 'http://lookalike.example', admin: '', all: ['http://10.0.0.9:9000/'] }, addresses: ['http://10.0.0.8:9000/'] }
    expect(peerEndpoint(peer)).toBe('http://10.0.0.8:9000')
    const prefill = prefillFromPeer(peer)!
    expect(prefill).toEqual({ endpoint: 'http://10.0.0.8:9000', name: 'other', region: 'eu', serverId: 'p1', kind: 'gos3', forcePathStyle: true, source: 'discovered' })
    // No credentials ride along with a discovery prefill.
    expect(prefill.accessKeyId).toBeUndefined()
    expect(prefill.secretAccessKey).toBeUndefined()
    // The advertised endpoint is only a hint, and only when it differs.
    expect(peerAdvertisedEndpoint(peer, prefill.endpoint)).toBe('http://lookalike.example')
    expect(peerAdvertisedEndpoint({ endpoints: { s3: 'http://10.0.0.8:9000/', admin: '' } }, prefill.endpoint)).toBeUndefined()
    // Advertised endpoints alone are not usable.
    expect(prefillFromPeer({ ...peer, addresses: [] })).toBeUndefined()
    // A region that could not be signed with (or put into a config file) is dropped.
    expect(prefillFromPeer({ ...peer, region: 'eu\n[default]' })?.region).toBeUndefined()
    // Malformed documents: non-array lists are ignored.
    const odd = { ...peer, endpoints: { s3: '', admin: '', all: 'http://x:9000' as unknown as string[] }, addresses: 'http://10.0.0.8:9000' as unknown as string[] }
    expect(peerEndpoint(odd)).toBeUndefined()
    expect(peerAdvertisedEndpoint(odd, undefined)).toBeUndefined()
    expect(prefillFromPeer(odd)).toBeUndefined()
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

  it('rejects redirects with dot segments, encoded slashes or control characters', () => {
    // The audit's link: router.replace decodes %2E%2E to a bucket named '..'.
    expect(safeRedirect('/servers/this-server/buckets/%2E%2E/objects/?key=_gos3/admin/v1/users/alice')).toBeUndefined()
    expect(safeRedirect('/servers/x/buckets/%252e%252e/objects/')).toBeUndefined()
    expect(safeRedirect('/servers/x/buckets/../../evil')).toBeUndefined()
    expect(safeRedirect('/servers/x/buckets/./b')).toBeUndefined()
    expect(safeRedirect('/servers/x/buckets/a%2Fb')).toBeUndefined()
    expect(safeRedirect('/servers/x/buckets/a%5C..')).toBeUndefined()
    expect(safeRedirect('/servers\\x')).toBeUndefined()
    expect(safeRedirect('/servers/x\nSet-Cookie')).toBeUndefined()
    // Ordinary in-app routes still pass, including encoded names and dots in names.
    expect(safeRedirect('/servers/x/buckets/my.bucket/objects/a%20b/100%25.txt')).toBe('/servers/x/buckets/my.bucket/objects/a%20b/100%25.txt')
    expect(safeRedirect('/servers/x/buckets/b/objects/?key=a%2F..%2Fb')).toBe('/servers/x/buckets/b/objects/?key=a%2F..%2Fb')
    expect(safeRedirect('/servers/x/buckets/b?prefix=..%2F')).toBe('/servers/x/buckets/b?prefix=..%2F')
  })
})

describe('credential routing', () => {
  const conn = (over: Partial<Connection>): Connection => ({ ...CONN, accessKeyId: '', secretAccessKey: '', ...over })

  it('never puts verified credentials on a connection that only shares the server ID (web-auth-1)', () => {
    // An attacker entry (import file / spoofed peer) claims prod's server ID.
    const evil = conn({ id: 'evil', endpoint: 'http://evil:9000', serverId: 'prod-id' })
    // The user typed and verified prod's endpoint, whose /_gos3/info reports prod-id.
    expect(reusableConnection([evil], { endpoint: 'https://prod:9000', serverId: 'prod-id', accessKeyId: 'ROOTKEY' })).toBeUndefined()
    // Same with the same access key on the attacker entry.
    expect(reusableConnection([conn({ ...evil, accessKeyId: 'ROOTKEY' })], { endpoint: 'https://prod:9000', serverId: 'prod-id', accessKeyId: 'ROOTKEY' })).toBeUndefined()
  })

  it('reuses a connection for the verified endpoint (same server ID first)', () => {
    const a = conn({ id: 'a', endpoint: 'https://prod:9000', serverId: 'other' })
    const b = conn({ id: 'b', endpoint: 'https://PROD:9000/', serverId: 'prod-id' })
    expect(reusableConnection([a, b], { endpoint: 'https://prod:9000', serverId: 'prod-id', accessKeyId: 'K' })?.id).toBe('b')
    expect(reusableConnection([a], { endpoint: 'https://prod:9000', serverId: 'prod-id', accessKeyId: 'K' })?.id).toBe('a')
    // A connection with another identity's keys is left alone (a new one is added).
    expect(reusableConnection([conn({ ...a, accessKeyId: 'OTHER', secretAccessKey: 'S' })], { endpoint: 'https://prod:9000', accessKeyId: 'K' })).toBeUndefined()
    expect(reusableConnection([conn({ ...a, accessKeyId: 'K', secretAccessKey: 'OLD' })], { endpoint: 'https://prod:9000', accessKeyId: 'K' })?.id).toBe('a')
  })

  it('flags link-prefilled endpoints that are neither this site nor a configured server', () => {
    const known = [conn({ endpoint: 'http://nas:9000' }), conn({ endpoint: 'https://s3.example.com' })]
    expect(linkPrefillCheck('https://gos3-corp.attacker.tld', 'https://gos3.corp', known)).toEqual({ origin: 'https://gos3-corp.attacker.tld', unknown: true, downgrade: false })
    expect(linkPrefillCheck('https://GOS3.corp/', 'https://gos3.corp', known)).toEqual({ origin: 'https://gos3.corp', unknown: false, downgrade: false })
    expect(linkPrefillCheck('nas:9000', 'http://ui.local', known)).toEqual({ origin: 'http://nas:9000', unknown: false, downgrade: false })
    expect(linkPrefillCheck('https://s3.example.com:443/', 'http://ui.local', known)).toMatchObject({ unknown: false })
    // Scheme and port count, not just the host name.
    expect(linkPrefillCheck('http://nas:9001', 'http://ui.local', known)).toMatchObject({ unknown: true, downgrade: false })
    expect(linkPrefillCheck('https://nas:9000', 'http://ui.local', known)).toMatchObject({ unknown: true, downgrade: false })
    expect(linkPrefillCheck('not a url', 'http://ui.local', known)).toBeUndefined()
  })

  it('treats an https-to-http downgrade of a known host as unknown', () => {
    const known = [conn({ endpoint: 'https://s3.example.com' })]
    expect(linkPrefillCheck('http://s3.example.com', 'http://ui.local', known)).toEqual({ origin: 'http://s3.example.com', unknown: true, downgrade: true })
    // Also against the page itself (embedded UI on https).
    expect(linkPrefillCheck('http://gos3.corp:9000', 'https://gos3.corp', [])).toMatchObject({ unknown: true, downgrade: true })
  })

  it('names the host that would receive reused credentials', () => {
    const from = conn({ endpoint: 'http://10.0.0.5:9000', accessKeyId: 'AK', secretAccessKey: 'SK' })
    expect(credentialReuseHost('http://10.0.0.66:9000', from)).toBe('10.0.0.66:9000')
    expect(credentialReuseHost('http://10.0.0.5:9000/', from)).toBeUndefined()
    expect(credentialReuseHost('', from)).toBeUndefined()
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
