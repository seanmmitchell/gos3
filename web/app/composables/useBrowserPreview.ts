// Object browser: preview helpers — what kind of preview an object gets, and a
// tiny, dependency-free Markdown renderer whose output is safe for v-html
// (all text is HTML-escaped; only a fixed set of tags is generated and links
// are restricted to http(s), mailto and relative URLs).
import { extName } from '~/utils/paths'

export type PreviewKind = 'image' | 'video' | 'audio' | 'markdown' | 'json' | 'text' | 'none'

/** Max size for in-browser image/video/audio previews via presigned URL. */
export const PREVIEW_MEDIA_MAX_BYTES = 50 * 1024 * 1024
/** Bytes fetched (range GET) for text previews. */
export const PREVIEW_TEXT_MAX_BYTES = 256 * 1024
/** Deepest block quote nesting renderMarkdown renders as <blockquote>. */
export const MARKDOWN_MAX_QUOTE_DEPTH = 16

const IMAGE_EXT = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'avif', 'bmp', 'ico', 'svg'])
const VIDEO_EXT = new Set(['mp4', 'webm', 'ogv', 'mov', 'm4v'])
const AUDIO_EXT = new Set(['mp3', 'wav', 'ogg', 'oga', 'flac', 'm4a', 'aac', 'opus'])
const MD_EXT = new Set(['md', 'markdown', 'mdown', 'mkd'])
const JSON_EXT = new Set(['json', 'geojson', 'jsonl', 'ndjson', 'har', 'webmanifest'])
const TEXT_EXT = new Set([
  'txt', 'log', 'csv', 'tsv', 'xml', 'yaml', 'yml', 'toml', 'ini', 'cfg', 'conf', 'env', 'properties',
  'html', 'htm', 'css', 'scss', 'less', 'js', 'mjs', 'cjs', 'ts', 'tsx', 'jsx', 'vue', 'svelte',
  'go', 'mod', 'sum', 'py', 'rb', 'rs', 'java', 'kt', 'c', 'h', 'cc', 'cpp', 'hpp', 'cs', 'php', 'pl', 'lua',
  'sh', 'bash', 'zsh', 'fish', 'ps1', 'bat', 'sql', 'graphql', 'proto', 'tf', 'hcl', 'dockerfile', 'makefile',
  'gitignore', 'editorconfig', 'rst', 'adoc', 'tex', 'srt', 'vtt', 'diff', 'patch', 'pem', 'crt', 'pub'
])

/** Chooses a preview type from the content type (preferred) and the extension. */
export function previewKind(key: string, contentType?: string): PreviewKind {
  const ct = (contentType ?? '').toLowerCase().split(';')[0]!.trim()
  const ext = extName(key)
  if (MD_EXT.has(ext) || ct === 'text/markdown') return 'markdown'
  if (ct.startsWith('image/') || IMAGE_EXT.has(ext)) return 'image'
  if (ct.startsWith('video/') || VIDEO_EXT.has(ext)) return 'video'
  if (ct.startsWith('audio/') || AUDIO_EXT.has(ext)) return 'audio'
  if (ct === 'application/json' || ct.endsWith('+json') || JSON_EXT.has(ext)) return 'json'
  if (
    ct.startsWith('text/')
    || ct === 'application/xml' || ct.endsWith('+xml')
    || ct === 'application/javascript' || ct === 'application/x-yaml' || ct === 'application/yaml'
    || ct === 'application/toml' || ct === 'application/x-sh'
    || TEXT_EXT.has(ext)
  ) return 'text'
  // Extension-less files such as README, LICENSE, Makefile.
  const base = key.slice(key.lastIndexOf('/') + 1).toLowerCase()
  if (['readme', 'license', 'licence', 'makefile', 'dockerfile', 'changelog', 'authors', 'notice'].includes(base)) return 'text'
  return 'none'
}

/** Pretty-prints JSON (or each line of JSON Lines); returns the input unchanged when it does not parse. */
export function prettyJson(text: string): { text: string, valid: boolean } {
  try {
    return { text: JSON.stringify(JSON.parse(text), null, 2), valid: true }
  } catch {
    const lines = text.split('\n').filter(l => l.trim())
    if (lines.length > 1) {
      try {
        return { text: lines.map(l => JSON.stringify(JSON.parse(l))).join('\n'), valid: true }
      } catch {
        // fall through
      }
    }
    return { text, valid: false }
  }
}

export function previewEscapeHtml(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;')
}

function safeHref(raw: string): string | undefined {
  const href = raw.trim()
  // URL parsers drop leading C0 controls / spaces and tabs or newlines
  // anywhere ('\u0001javascript:…' is a javascript: URL): refuse them all.
  // eslint-disable-next-line no-control-regex
  if (/[\x00-\x20\x7f]/.test(href)) return undefined
  if (/^(https?:|mailto:)/i.test(href)) return href
  // Relative links (no scheme at all; '\\' counts as '/' in http URLs).
  if (!/^[a-z][a-z0-9+.-]*:/i.test(href) && !/^[/\\]{2}/.test(href)) return href
  return undefined
}

/** Inline Markdown on already-escaped text: code, bold, italic, strike, links. */
function inline(escaped: string): string {
  const codes: string[] = []
  let s = escaped.replace(/`([^`]+)`/g, (_m, c: string) => {
    codes.push(`<code>${c}</code>`)
    return `\u0000${codes.length - 1}\u0000`
  })
  // The label cannot cross '[' and the URL holds at most one '(…)' group
  // ('…/Foo_(bar)'): a failed match then stops at the next bracket, which
  // keeps this linear in the input (with '[^\]]*' a run of '[' without ']'
  // took quadratic time).
  s = s.replace(/!?\[([^[\]]*)\]\(([^()\s]+(?:\([^()\s]*\))?)(?:\s+&quot;[^&]*&quot;)?\)/g, (_m, label: string, url: string) => {
    // url is escaped; unescape &amp; for the check, re-escape on output.
    const href = safeHref(url.replace(/&amp;/g, '&'))
    if (!href) return label
    return `<a href="${previewEscapeHtml(href)}" target="_blank" rel="noopener noreferrer nofollow">${label || previewEscapeHtml(href)}</a>`
  })
  s = s
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/__([^_]+)__/g, '<strong>$1</strong>')
    .replace(/(^|[^*])\*([^*\s][^*]*)\*/g, '$1<em>$2</em>')
    .replace(/(^|[^\w])_([^_\s][^_]*)_(?!\w)/g, '$1<em>$2</em>')
    .replace(/~~([^~]+)~~/g, '<del>$1</del>')
  // eslint-disable-next-line no-control-regex
  return s.replace(/\u0000(\d+)\u0000/g, (_m, i: string) => codes[Number(i)] ?? '')
}

/**
 * Renders a practical Markdown subset (headings, paragraphs, emphasis, code
 * spans and fenced blocks, block quotes, lists, horizontal rules, links) to
 * safe HTML. Block quotes nest at most MARKDOWN_MAX_QUOTE_DEPTH levels;
 * deeper quote markers are kept as text (a line of thousands of '>' would
 * otherwise recurse once per marker and overflow the stack).
 */
export function renderMarkdown(src: string, depth = 0): string {
  const lines = src.replace(/\r\n?/g, '\n').split('\n')
  const out: string[] = []
  let para: string[] = []
  let list: { tag: 'ul' | 'ol', items: string[] } | null = null
  let quote: string[] = []

  const flushPara = () => {
    if (para.length) out.push(`<p>${inline(previewEscapeHtml(para.join(' ')))}</p>`)
    para = []
  }
  const flushList = () => {
    if (list) out.push(`<${list.tag}>${list.items.map(i => `<li>${inline(previewEscapeHtml(i))}</li>`).join('')}</${list.tag}>`)
    list = null
  }
  const flushQuote = () => {
    if (quote.length) out.push(`<blockquote>${renderMarkdown(quote.join('\n'), depth + 1)}</blockquote>`)
    quote = []
  }
  const flushAll = () => {
    flushPara()
    flushList()
    flushQuote()
  }

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]!
    const fence = /^\s*(```|~~~)\s*([\w+-]*)\s*$/.exec(line)
    if (fence) {
      flushAll()
      const body: string[] = []
      i++
      while (i < lines.length && !lines[i]!.trim().startsWith(fence[1]!)) body.push(lines[i++]!)
      const lang = fence[2] ? ` data-lang="${previewEscapeHtml(fence[2])}"` : ''
      out.push(`<pre${lang}><code>${previewEscapeHtml(body.join('\n'))}</code></pre>`)
      continue
    }
    if (depth < MARKDOWN_MAX_QUOTE_DEPTH && /^\s*>/.test(line)) {
      flushPara()
      flushList()
      quote.push(line.replace(/^\s*>\s?/, ''))
      continue
    }
    flushQuote()
    if (!line.trim()) {
      flushPara()
      flushList()
      continue
    }
    const h = /^(#{1,6})\s+(.*?)\s*#*\s*$/.exec(line)
    if (h) {
      flushAll()
      const level = h[1]!.length
      out.push(`<h${level}>${inline(previewEscapeHtml(h[2]!))}</h${level}>`)
      continue
    }
    if (/^\s*([-*_])(\s*\1){2,}\s*$/.test(line)) {
      flushAll()
      out.push('<hr>')
      continue
    }
    const li = /^\s*(?:([-*+])|(\d+)[.)])\s+(.*)$/.exec(line)
    if (li) {
      flushPara()
      const tag = li[1] ? 'ul' : 'ol'
      if (list && list.tag !== tag) flushList()
      list ??= { tag, items: [] }
      list.items.push(li[3]!)
      continue
    }
    if (list && /^\s{2,}\S/.test(line)) {
      list.items[list.items.length - 1] += ` ${line.trim()}`
      continue
    }
    flushList()
    para.push(line.trim())
  }
  flushAll()
  return out.join('\n')
}
