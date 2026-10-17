// Namespace-agnostic XML helpers on top of DOMParser, plus a tiny escaping
// XML builder. S3 responses use the default namespace
// http://s3.amazonaws.com/doc/2006-03-01/ (or none, for some servers), so every
// lookup here matches on `localName` and ignores namespaces.

export const S3_XMLNS = 'http://s3.amazonaws.com/doc/2006-03-01/'

export class XmlParseError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'XmlParseError'
  }
}

/** Parses an XML document and returns its root element. */
export function parseXml(text: string): Element {
  if (typeof DOMParser === 'undefined') {
    throw new XmlParseError('DOMParser is not available in this environment')
  }
  const doc = new DOMParser().parseFromString(text.trim(), 'application/xml')
  const root = doc.documentElement
  if (!root || root.localName === 'parsererror' || doc.getElementsByTagName('parsererror').length > 0) {
    throw new XmlParseError('Invalid XML document')
  }
  return root
}

/** Like parseXml but returns undefined instead of throwing. */
export function tryParseXml(text: string): Element | undefined {
  try {
    return text.trim() ? parseXml(text) : undefined
  } catch {
    return undefined
  }
}

/** Direct element children of el, optionally filtered by local name. */
export function children(el: Element | undefined, name?: string): Element[] {
  if (!el) return []
  const out: Element[] = []
  for (let n = el.firstElementChild; n; n = n.nextElementSibling) {
    if (name === undefined || n.localName === name) out.push(n)
  }
  return out
}

/** First direct child element named name. */
export function child(el: Element | undefined, name: string): Element | undefined {
  if (!el) return undefined
  for (let n = el.firstElementChild; n; n = n.nextElementSibling) {
    if (n.localName === name) return n
  }
  return undefined
}

/** Follows a path of child names, e.g. path(el, 'Rule', 'DefaultRetention'). */
export function path(el: Element | undefined, ...names: string[]): Element | undefined {
  let cur = el
  for (const n of names) {
    cur = child(cur, n)
    if (!cur) return undefined
  }
  return cur
}

/** Text content of the element itself. */
export function textOf(el: Element | undefined): string | undefined {
  if (!el) return undefined
  return el.textContent ?? ''
}

/** Text of the first child named name (undefined when absent). */
export function text(el: Element | undefined, name: string): string | undefined {
  return textOf(child(el, name))
}

/** Text of every child named name. */
export function texts(el: Element | undefined, name: string): string[] {
  return children(el, name).map(e => e.textContent ?? '')
}

/** Integer value of child name, or undefined when absent / not a number. */
export function num(el: Element | undefined, name: string): number | undefined {
  const t = text(el, name)
  if (t === undefined || t.trim() === '') return undefined
  const n = Number(t.trim())
  return Number.isFinite(n) ? n : undefined
}

/** Boolean value of child name ("true"/"false"), or undefined when absent. */
export function bool(el: Element | undefined, name: string): boolean | undefined {
  const t = text(el, name)
  if (t === undefined) return undefined
  return t.trim().toLowerCase() === 'true'
}

/** Removes surrounding double quotes (and &quot; leftovers) from an ETag. */
export function unquoteETag(etag: string | null | undefined): string | undefined {
  if (etag == null) return undefined
  const t = etag.trim()
  if (t === '') return undefined
  return t.replace(/^W\//, '').replace(/^"+|"+$/g, '')
}

// ---------------------------------------------------------------------------
// Builder
// ---------------------------------------------------------------------------

/**
 * Escapes text for use in XML character data and attribute values. Tab, LF
 * and CR are written as character references, so line-ending and attribute
 * whitespace normalization cannot change them. Characters XML 1.0 cannot
 * represent at all (see hasXmlIllegalChars) are left as they are.
 */
export function escapeXml(value: string): string {
  return value
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&apos;')
    .replace(/\t/g, '&#9;')
    .replace(/\n/g, '&#10;')
    .replace(/\r/g, '&#13;')
}

/** A character outside the XML 1.0 Char production (lone surrogates included). */
const XML_ILLEGAL_CHAR = /[^\t\n\r -퟿-�\u{10000}-\u{10FFFF}]/u

/**
 * True when value contains a character that XML 1.0 cannot carry, not even
 * as a character reference (e.g. U+0001): a document containing it is
 * rejected by the server's parser.
 */
export function hasXmlIllegalChars(value: string): boolean {
  return XML_ILLEGAL_CHAR.test(value)
}

type Leaf = string | number | boolean | null | undefined

/**
 * Text element: `t('Key', 'a&b')` → `<Key>a&amp;b</Key>`.
 * Returns '' when value is null/undefined so optional fields can be passed
 * straight through.
 */
export function t(name: string, value: Leaf): string {
  if (value === null || value === undefined) return ''
  return `<${name}>${escapeXml(String(value))}</${name}>`
}

/**
 * Container element whose children are already-built XML strings:
 * `x('Tag', t('Key', k), t('Value', v))`. Empty strings are dropped; arrays
 * are flattened.
 */
export function x(name: string, ...content: (string | string[] | undefined | null | false)[]): string {
  const inner = content.flat().filter((c): c is string => typeof c === 'string').join('')
  return `<${name}>${inner}</${name}>`
}

/** Document with XML declaration and the S3 namespace on the root element. */
export function xmlDocument(root: string, ...content: (string | string[] | undefined | null | false)[]): string {
  const inner = content.flat().filter((c): c is string => typeof c === 'string').join('')
  return `<?xml version="1.0" encoding="UTF-8"?>\n<${root} xmlns="${S3_XMLNS}">${inner}</${root}>`
}
