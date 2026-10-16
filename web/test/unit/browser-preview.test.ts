import { describe, expect, it } from 'vitest'
import { MARKDOWN_MAX_QUOTE_DEPTH, PREVIEW_TEXT_MAX_BYTES, prettyJson, previewKind, renderMarkdown } from '~/composables/useBrowserPreview'

describe('previewKind', () => {
  it('uses the content type, then the extension', () => {
    expect(previewKind('x.bin', 'image/png')).toBe('image')
    expect(previewKind('photo.JPG')).toBe('image')
    expect(previewKind('a.json', 'application/octet-stream')).toBe('json')
    expect(previewKind('a', 'application/vnd.api+json')).toBe('json')
    expect(previewKind('README.md', 'text/plain')).toBe('markdown')
    expect(previewKind('main.go')).toBe('text')
    expect(previewKind('dir/LICENSE')).toBe('text')
    expect(previewKind('movie.webm')).toBe('video')
    expect(previewKind('song.mp3')).toBe('audio')
    expect(previewKind('archive.zip', 'application/zip')).toBe('none')
  })
})

describe('prettyJson', () => {
  it('pretty-prints JSON and JSON Lines, leaves invalid text alone', () => {
    expect(prettyJson('{"a":1}')).toEqual({ text: '{\n  "a": 1\n}', valid: true })
    expect(prettyJson('{"a":1}\n{"b":2}\n')).toEqual({ text: '{"a":1}\n{"b":2}', valid: true })
    expect(prettyJson('{oops')).toEqual({ text: '{oops', valid: false })
  })
})

describe('renderMarkdown', () => {
  it('renders the common subset', () => {
    const html = renderMarkdown('# Title\n\nSome **bold** and *em* and `code`.\n\n- one\n- two\n\n1. first\n\n```js\nlet a = 1 < 2\n```\n\n> quote\n\n---\n[link](https://example.com)')
    expect(html).toContain('<h1>Title</h1>')
    expect(html).toContain('<strong>bold</strong>')
    expect(html).toContain('<em>em</em>')
    expect(html).toContain('<code>code</code>')
    expect(html).toContain('<ul><li>one</li><li>two</li></ul>')
    expect(html).toContain('<ol><li>first</li></ol>')
    expect(html).toContain('<pre data-lang="js"><code>let a = 1 &lt; 2</code></pre>')
    expect(html).toContain('<blockquote><p>quote</p></blockquote>')
    expect(html).toContain('<hr>')
    expect(html).toContain('<a href="https://example.com" target="_blank" rel="noopener noreferrer nofollow">link</a>')
  })

  it('escapes HTML and drops dangerous links', () => {
    const html = renderMarkdown('<script>alert(1)</script>\n\n<img src=x onerror=alert(1)>\n\n[x](javascript:alert(1)) [y](data:text/html,hi) [rel](../other.md)')
    expect(html).not.toContain('<script')
    expect(html).not.toContain('<img')
    expect(html).toContain('&lt;script&gt;')
    expect(html).not.toContain('javascript:')
    expect(html).not.toContain('href="data:')
    expect(html).toContain('href="../other.md"')
  })

  it('does not apply emphasis inside code spans', () => {
    expect(renderMarkdown('`a*b*c` and snake_case_name')).toBe('<p><code>a*b*c</code> and snake_case_name</p>')
  })
})

describe('renderMarkdown tables', () => {
  it('renders a GFM table with alignment and inline Markdown', () => {
    const html = renderMarkdown('Intro\n| Name | Size | Note |\n| :--- | ---: | :-: |\n| `a.txt` | 12 | **new** |\n| b\\|c | 3 |\n\nAfter')
    expect(html).toBe([
      '<p>Intro</p>',
      '<table><thead><tr><th style="text-align:left">Name</th><th style="text-align:right">Size</th><th style="text-align:center">Note</th></tr></thead>'
      + '<tbody><tr><td style="text-align:left"><code>a.txt</code></td><td style="text-align:right">12</td><td style="text-align:center"><strong>new</strong></td></tr>'
      + '<tr><td style="text-align:left">b|c</td><td style="text-align:right">3</td><td style="text-align:center"></td></tr></tbody></table>',
      '<p>After</p>'
    ].join('\n'))
  })

  it('accepts rows without outer pipes and ends at the next block', () => {
    const html = renderMarkdown('a | b\n--|--\n1 | 2 | extra\n# Next')
    expect(html).toBe('<table><thead><tr><th>a</th><th>b</th></tr></thead><tbody><tr><td>1</td><td>2</td></tr></tbody></table>\n<h1>Next</h1>')
  })

  it('needs a matching delimiter row', () => {
    expect(renderMarkdown('a | b\n--- | --- | ---')).not.toContain('<table')
    expect(renderMarkdown('| a | b |\n| x | y |')).not.toContain('<table')
    expect(renderMarkdown('text\n---')).toBe('<p>text</p>\n<hr>')
  })

  it('escapes cell contents', () => {
    const html = renderMarkdown('| <b>x</b> |\n|---|\n| <img src=x onerror=alert(1)> |')
    expect(html).not.toContain('<img')
    expect(html).toContain('<th>&lt;b&gt;x&lt;/b&gt;</th>')
  })

  it('checks long delimiter-like lines in linear time', () => {
    const start = performance.now()
    renderMarkdown(`a | b\n|${' '.repeat(100_000)}x`)
    expect(performance.now() - start).toBeLessThan(500)
  })
})

describe('renderMarkdown link filter (review fix)', () => {
  it('drops links that URL parsers turn into javascript: URLs', () => {
    for (const c of ['\u0001', '\u0008', '\u000e', '\u001f', '\u007f']) {
      const html = renderMarkdown(`[click](${c}javascript:alert(document.domain))`)
      expect(html, JSON.stringify(c)).not.toContain('<a')
      // What a browser would make of the href, had it been kept.
      if (c < ' ') expect(new URL(`${c}javascript:alert(1)`, 'http://ui.local/').protocol).toBe('javascript:')
    }
    expect(renderMarkdown('[x](\\\\evil.example/a)')).not.toContain('<a')
  })

  it('keeps ordinary links', () => {
    expect(renderMarkdown('[a](https://example.com/x?y=1&z=2) [b](docs/readme.md) [c](mailto:me@example.com)'))
      .toBe('<p><a href="https://example.com/x?y=1&amp;z=2" target="_blank" rel="noopener noreferrer nofollow">a</a> <a href="docs/readme.md" target="_blank" rel="noopener noreferrer nofollow">b</a> <a href="mailto:me@example.com" target="_blank" rel="noopener noreferrer nofollow">c</a></p>')
  })
})

describe('renderMarkdown on hostile input (web-content-2)', () => {
  const n = PREVIEW_TEXT_MAX_BYTES
  // Each of these took quadratic time or overflowed the stack before the fix
  // ('['.repeat(256 KiB) took about 30 s). Linear rendering is well under a second.
  const inputs: Record<string, string> = {
    'brackets': '['.repeat(n),
    'brackets with text': '[a'.repeat(n / 2),
    'empty links without a closing paren': '[](x'.repeat(n / 4),
    'link prefix': '[](' + 'a'.repeat(n),
    'image prefixes': '![x]('.repeat(n / 5),
    'titles': '[a](b "'.repeat(n / 7),
    'emphasis': '*a'.repeat(n / 2),
    'underscores': '_a'.repeat(n / 2),
    'heading spaces': '# ' + ' '.repeat(n) + 'x',
    'rule-like line': '- '.repeat(n / 2) + 'x',
    'list items': '- [\n'.repeat(n / 4)
  }
  for (const [name, src] of Object.entries(inputs)) {
    it(`renders ${name} in linear time`, () => {
      const t0 = performance.now()
      const html = renderMarkdown(src)
      expect(performance.now() - t0, name).toBeLessThan(1500)
      expect(html).not.toContain('<script')
    })
  }

  it('caps block quote nesting instead of recursing per marker', () => {
    expect(() => renderMarkdown('>'.repeat(50000) + ' deep')).not.toThrow()
    const html = renderMarkdown('>'.repeat(40) + ' deep')
    expect(html.match(/<blockquote>/g)).toHaveLength(MARKDOWN_MAX_QUOTE_DEPTH)
    expect(html).toContain('&gt; deep') // the rest stays text
    expect(renderMarkdown('> a\n>> b')).toBe('<blockquote><p>a</p>\n<blockquote><p>b</p></blockquote></blockquote>')
  })

  it('still renders links with one level of parentheses', () => {
    expect(renderMarkdown('[w](https://en.wikipedia.org/wiki/Foo_(bar))')).toContain('href="https://en.wikipedia.org/wiki/Foo_(bar)"')
    expect(renderMarkdown('[a](b "t")')).toContain('href="b"')
  })
})
