import { describe, expect, it } from 'vitest'
import { prettyJson, previewKind, renderMarkdown } from '~/composables/useBrowserPreview'

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
