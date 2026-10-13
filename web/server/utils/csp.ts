// Security headers of the standalone (Node) console, the counterpart of the
// embedded UI's headers in internal/webui (webui.go, csp.go): the same
// Content-Security-Policy directives, X-Frame-Options, Referrer-Policy and
// nosniff. nuxt.config.ts sends SECURITY_HEADERS on every response
// (routeRules); server/plugins/csp.ts replaces the policy of each rendered
// HTML page with one that also allows that page's inline scripts by hash.
//
// The UI keeps S3 secret keys in Web Storage, so the policy is what contains
// a script-injection bug instead of letting it read them:
//   - script-src: the UI's own files plus the SHA-256 hashes of the page's
//     inline scripts (Nuxt's runtime config and the color-mode bootstrap);
//     no 'unsafe-inline', no 'unsafe-eval'.
//   - connect-src stays open to any http(s) origin: the UI talks to every S3
//     endpoint the user adds, cross-origin.
//   - img-src / media-src allow any http(s) origin and blob:/data: for
//     object previews; style-src needs 'unsafe-inline' (inline styles).
//   - object-src, base-uri and form-action are 'none'; frame-ancestors
//     'none' forbids framing (clickjacking).
import { createHash } from 'node:crypto'

const CSP_AFTER_SCRIPTS = "; style-src 'self' 'unsafe-inline'" +
  "; img-src 'self' data: blob: http: https:" +
  "; media-src 'self' blob: http: https:" +
  "; font-src 'self' data:" +
  "; connect-src 'self' http: https:" +
  "; worker-src 'self'" +
  "; manifest-src 'self'" +
  "; object-src 'none'" +
  "; base-uri 'none'" +
  "; form-action 'none'" +
  "; frame-ancestors 'none'"

/** The Content-Security-Policy allowing the given script hash sources ("sha256-…"). */
export function contentSecurityPolicy(hashes: Iterable<string> = []): string {
  let p = "default-src 'self'; script-src 'self'"
  for (const h of [...new Set(hashes)].sort()) p += ` '${h}'`
  return p + CSP_AFTER_SCRIPTS
}

/** Headers of every response of the standalone server. */
export const SECURITY_HEADERS: Record<string, string> = {
  'Content-Security-Policy': contentSecurityPolicy(),
  'X-Frame-Options': 'DENY',
  'Referrer-Policy': 'no-referrer',
  'X-Content-Type-Options': 'nosniff',
  'Permissions-Policy': 'camera=(), microphone=(), geolocation=(), payment=(), usb=()'
}

/** True when a <script type> value makes the element something script-src governs. */
function executableScriptType(typ: string): boolean {
  let t = typ.trim().toLowerCase()
  if (t === '' || t === 'module' || t === 'importmap' || t === 'speculationrules') return true
  const semi = t.indexOf(';')
  if (semi >= 0) t = t.slice(0, semi).trim()
  return /^(?:text|application)\/(?:x-)?(?:java|ecma)script$/.test(t) || /^text\/(?:javascript1\.[0-5]|jscript|livescript)$/.test(t)
}

function attr(attrs: string, name: string): string | undefined {
  const m = new RegExp(`(?:^|\\s)${name}(?:\\s*=\\s*(?:"([^"]*)"|'([^']*)'|([^\\s"'=<>\`]+)))?(?=\\s|/|$)`, 'i').exec(attrs)
  return m ? (m[1] ?? m[2] ?? m[3] ?? '') : undefined
}

/**
 * CSP hash sources of the inline scripts in an HTML fragment: elements
 * without src whose type is JavaScript, a module or an import map (data
 * blocks such as type="application/json" never run). The hash covers the
 * element content as browsers see it: the HTML parser turns CRLF and lone
 * CR into LF before the script text exists.
 */
export function inlineScriptHashes(html: string): string[] {
  const out = new Set<string>()
  for (const m of html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script\s*>/gi)) {
    const attrs = m[1] ?? ''
    if (attr(attrs, 'src') !== undefined || !executableScriptType(attr(attrs, 'type') ?? '')) continue
    const text = (m[2] ?? '').replace(/\r\n?/g, '\n')
    out.add(`sha256-${createHash('sha256').update(text, 'utf8').digest('base64')}`)
  }
  return [...out]
}
