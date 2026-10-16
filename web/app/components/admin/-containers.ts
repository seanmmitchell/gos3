// Helpers for the containers of an s3 backing's wrapped buckets (backing
// page, "Containers" tab; AddContainerModal; the container pickers of the
// wrapped-bucket and move dialogs): state badges, removal rules, form
// validation and the ContainerSpec request body (docs/admin-api.md, "Remote
// buckets and containers (s3 backings)").
//
// The file name starts with "-" so Nuxt does not register it as a component.
import type { ContainerInfo, ContainerRole, ContainerSpec, ContainerState, RemoteLocation } from '~/types/admin'
import { containerItems } from '~/utils/backings'
import { validateLabel, validatePrefix, validateRegion, validateRemoteBucketName } from './-remote-buckets'

type BadgeColor = 'primary' | 'secondary' | 'success' | 'info' | 'warning' | 'error' | 'neutral'

export interface ContainerStateMeta {
  label: string
  color: BadgeColor
  icon: string
  description: string
}

export const CONTAINER_STATES: Readonly<Record<ContainerState, ContainerStateMeta>> = {
  loading: { label: 'Loading', color: 'info', icon: 'i-lucide-loader-circle', description: 'gos3 is reading the container; its buckets are available once it is ready.' },
  ready: { label: 'Ready', color: 'success', icon: 'i-lucide-circle-check', description: 'Its buckets are available.' },
  failed: { label: 'Failed', color: 'error', icon: 'i-lucide-triangle-alert', description: 'gos3 could not open the container; its buckets are unavailable.' }
}

/** Badge metadata of a container state (unknown states are neutral). */
export function containerStateMeta(state: ContainerState | string): ContainerStateMeta {
  return CONTAINER_STATES[state as ContainerState] ?? { label: state || 'Unknown', color: 'neutral', icon: 'i-lucide-circle-help', description: '' }
}

export const CONTAINER_ROLES: Readonly<Record<ContainerRole, { label: string, description: string }>> = {
  home: { label: 'Home', description: 'The backing\'s shared container, next to its records.' },
  dedicated: { label: 'Dedicated bucket', description: 'A remote bucket gos3 created for it.' },
  existing: { label: 'Existing bucket', description: 'A prefix of an existing remote bucket.' }
}

export function containerRoleLabel(role: ContainerRole | string): string {
  return CONTAINER_ROLES[role as ContainerRole]?.label ?? role
}

/** "s3://bucket/prefix" of a remote location ('' without one). */
export function remoteLocationUri(loc: RemoteLocation | null | undefined): string {
  if (!loc?.bucket) return ''
  return `s3://${loc.bucket}/${loc.prefix ?? ''}`
}

/** A container's display name: its label, else its ID ("shared (home)" for an unlabelled home). */
export function containerName(c: Pick<ContainerInfo, 'id' | 'label'>): string {
  return c.label || (c.id === 'home' ? 'shared (home)' : c.id)
}

/** The container called `id` in a list. */
export function findContainer(list: readonly ContainerInfo[] | null | undefined, id: string | undefined): ContainerInfo | undefined {
  return id ? list?.find(c => c.id === id) : undefined
}

/**
 * Why a container cannot be removed ('' when it can): the home is part of
 * the backing, only empty containers can be forgotten (their remote data is
 * left alone), and the default container must be replaced first (the server
 * refuses all three).
 */
export function containerRemoveRefusal(c: Pick<ContainerInfo, 'id' | 'role' | 'buckets' | 'default'>): string {
  if (c.role === 'home' || c.id === 'home') return 'The home container is part of the backing and cannot be removed.'
  if (c.buckets > 0) return `It holds ${c.buckets} wrapped bucket${c.buckets === 1 ? '' : 's'}; delete or move ${c.buckets === 1 ? 'it' : 'them'} first.`
  if (c.default) return 'It is the default container; make another one the default first.'
  return ''
}

/** Whether a container can become the default (ready and not the default already). */
export function canBeDefault(c: Pick<ContainerInfo, 'default' | 'state'>): boolean {
  return !c.default && c.state === 'ready'
}

export interface ContainerSelectItem {
  label: string
  value: string
  description?: string
  disabled?: boolean
}

/**
 * USelect items for choosing a container (wrapped-bucket and move dialogs):
 * the default marked, containers that are not ready disabled, the location
 * as description.
 */
export function containerSelectItems(list: readonly ContainerInfo[] | null | undefined): ContainerSelectItem[] {
  const byId = new Map((list ?? []).map(c => [c.id, c]))
  return containerItems({ containers: list }).map((item) => {
    const c = byId.get(item.value)!
    const out: ContainerSelectItem = { ...item, description: remoteLocationUri(c.location) || item.description }
    if (c.state !== 'ready') {
      out.disabled = true
      out.label = `${item.label} (${containerStateMeta(c.state).label.toLowerCase()})`
    }
    return out
  })
}

/** The default container if it is ready, else the first ready one ('' when none). */
export function preferredContainer(list: readonly ContainerInfo[] | null | undefined, defaultId?: string): string {
  const ready = (list ?? []).filter(c => c.state === 'ready')
  return ready.find(c => c.id === defaultId)?.id ?? ready.find(c => c.default)?.id ?? ready[0]?.id ?? ''
}

// --- add container form ------------------------------------------------------------

export interface ContainerForm {
  /** The remote bucket. */
  bucket: string
  /** Prefix inside it ("" = the whole bucket). */
  prefix: string
  /** Region of the remote bucket ("" = discovered / the backing's). */
  region: string
  /** existing: the remote bucket exists; create: gos3 creates it (a dedicated container). */
  source: 'existing' | 'create'
  /** existing only: the location already holds a gos3 store (e.g. moved from another server). */
  attach: boolean
  label: string
}

export function emptyContainerForm(over: Partial<ContainerForm> = {}): ContainerForm {
  return { bucket: '', prefix: '', region: '', source: 'existing', attach: false, label: '', ...over }
}

export interface ContainerFormErrors {
  bucket?: string
  prefix?: string
  region?: string
  label?: string
}

export function validateContainerForm(f: ContainerForm): ContainerFormErrors {
  const e: ContainerFormErrors = {}
  const bucket = validateRemoteBucketName(f.bucket.trim(), f.source === 'create')
  if (bucket) e.bucket = bucket
  const prefix = validatePrefix(f.prefix.trim())
  if (prefix) e.prefix = prefix
  const region = validateRegion(f.region.trim())
  if (region) e.region = region
  const label = validateLabel(f.label.trim())
  if (label) e.label = label
  return e
}

/** POST /backings/{name}/containers body of a form (empty fields left out). */
export function containerSpec(f: ContainerForm): ContainerSpec {
  const spec: ContainerSpec = { bucket: f.bucket.trim() }
  if (f.prefix.trim()) spec.prefix = f.prefix.trim()
  if (f.region.trim()) spec.region = f.region.trim()
  if (f.source === 'create') spec.createBucket = true
  else if (f.attach) spec.attach = true
  if (f.label.trim()) spec.label = f.label.trim()
  return spec
}
