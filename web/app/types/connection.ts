import type { Info, WhoAmI } from './admin'

export type ConnectionKind = 'gos3' | 's3'

/**
 * Where a connection came from:
 * - user:       added in the UI (or imported)
 * - preset:     runtime config NUXT_PUBLIC_SERVERS or public/servers.json
 * - embedded:   the gos3 server hosting this UI (same origin)
 * - discovered: added from a discovery result
 */
export type ConnectionSource = 'user' | 'preset' | 'embedded' | 'discovered'

export interface Connection {
  /** Stable local ID (used in routes: /servers/:id/...). */
  id: string
  name: string
  /** Base URL without trailing slash, e.g. "http://10.0.0.5:9000". */
  endpoint: string
  /** Signing region (default "us-east-1"). */
  region: string
  accessKeyId: string
  /** Never written to localStorage unless remember = true. */
  secretAccessKey: string
  kind: ConnectionKind
  /** Path-style addressing (recommended for gos3 / IP endpoints). */
  forcePathStyle: boolean
  /** Persist the secret in localStorage (else sessionStorage only). */
  remember: boolean
  source: ConnectionSource
  /** gos3 server ID (Info.id) once known. */
  serverId?: string
  /** Optional accent color name (Nuxt UI color or hex). */
  color?: string
}

/** Fields accepted when adding/importing a connection. */
export interface ConnectionInput {
  id?: string
  name?: string
  endpoint: string
  region?: string
  accessKeyId?: string
  secretAccessKey?: string
  kind?: ConnectionKind
  forcePathStyle?: boolean
  remember?: boolean
  source?: ConnectionSource
  serverId?: string
  color?: string
}

export type ConnectionState = 'unknown' | 'checking' | 'online' | 'offline' | 'unauthorized' | 'error'

export interface ConnectionStatus {
  state: ConnectionState
  /** Public identity (gos3 only; also filled for 's3' connections that turn out to be gos3). */
  info: Info | null
  /** Admin whoami (gos3 with credentials only). */
  whoami: WhoAmI | null
  error?: string
  errorCode?: string
  /** Epoch ms of the last completed check. */
  checkedAt?: number
  latencyMs?: number
}

/** Export file format (also accepted by import; a plain array works too). */
export interface ConnectionsExport {
  type: 'gos3-connections'
  version: 1
  exportedAt: string
  connections: ConnectionInput[]
}
