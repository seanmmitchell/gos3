import { describe, expect, it } from 'vitest'
import { bytesToHex, crc32, crc32Base64, md5Base64, md5Hex } from '~/utils/s3/hash'
import { bool, child, children, escapeXml, hasXmlIllegalChars, num, parseXml, t, text, texts, unquoteETag, x, xmlDocument, XmlParseError } from '~/utils/s3/xml'

describe('xml helpers', () => {
  it('parses namespaced and non-namespaced documents alike', () => {
    for (const ns of ['', ' xmlns="http://s3.amazonaws.com/doc/2006-03-01/"']) {
      const root = parseXml(`<?xml version="1.0" encoding="UTF-8"?><Root${ns}><A>1</A><B>true</B><C>x</C><C>y</C><D><E>deep</E></D></Root>`)
      expect(root.localName).toBe('Root')
      expect(num(root, 'A')).toBe(1)
      expect(bool(root, 'B')).toBe(true)
      expect(texts(root, 'C')).toEqual(['x', 'y'])
      expect(text(child(root, 'D'), 'E')).toBe('deep')
      expect(children(root).length).toBe(5)
      expect(text(root, 'Missing')).toBeUndefined()
      expect(num(root, 'Missing')).toBeUndefined()
    }
  })

  it('throws on invalid XML', () => {
    expect(() => parseXml('<a><b></a>')).toThrow(XmlParseError)
  })

  it('escapes text and builds documents', () => {
    expect(escapeXml(`a&b<c>"d"'e'`)).toBe('a&amp;b&lt;c&gt;&quot;d&quot;&apos;e&apos;')
    expect(t('Key', 'a&b')).toBe('<Key>a&amp;b</Key>')
    expect(t('Key', undefined)).toBe('')
    expect(x('Tag', t('Key', 'k'), t('Value', 'v'))).toBe('<Tag><Key>k</Key><Value>v</Value></Tag>')
    const doc = xmlDocument('Tagging', x('TagSet'))
    expect(doc).toContain('<Tagging xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><TagSet></TagSet></Tagging>')
    // round trip through the parser
    const weird = '<&>"\' ünï 🚀'
    expect(text(parseXml(xmlDocument('R', t('V', weird))), 'V')).toBe(weird)
  })

  it('writes tab, LF and CR as character references so they survive parsing', () => {
    expect(escapeXml('a\tb\nc\rd')).toBe('a&#9;b&#10;c&#13;d')
    const ws = ' a\r\nb\rc\td \n'
    expect(text(parseXml(xmlDocument('R', t('V', ws))), 'V')).toBe(ws)
  })

  it('detects characters XML 1.0 cannot carry', () => {
    for (const ok of ['', 'plain/key.txt', 'tab\tlf\ncr\r', 'ünï 🚀', '퟿�', '\u{10FFFF}']) {
      expect(hasXmlIllegalChars(ok)).toBe(false)
    }
    for (const bad of ['a\u0001b', '\u0000', 'x\u001F', '\u000B', '￾', '￿', 'lone \uD800 surrogate', '\uDC00']) {
      expect(hasXmlIllegalChars(bad)).toBe(true)
    }
  })

  it('unquotes ETags', () => {
    expect(unquoteETag('"abc"')).toBe('abc')
    expect(unquoteETag('W/"abc-2"')).toBe('abc-2')
    expect(unquoteETag('abc')).toBe('abc')
    expect(unquoteETag(null)).toBeUndefined()
    expect(unquoteETag('')).toBeUndefined()
  })
})

describe('hash', () => {
  it('computes MD5 test vectors (RFC 1321)', () => {
    expect(md5Hex('')).toBe('d41d8cd98f00b204e9800998ecf8427e')
    expect(md5Hex('a')).toBe('0cc175b9c0f1b6a831c399e269772661')
    expect(md5Hex('abc')).toBe('900150983cd24fb0d6963f7d28e17f72')
    expect(md5Hex('message digest')).toBe('f96b697d7cb7938d525a2f31aaf161d0')
    expect(md5Hex('12345678901234567890123456789012345678901234567890123456789012345678901234567890')).toBe('57edf4a22be3c955ac49da2e2107b67a')
    expect(md5Hex('a'.repeat(1000))).toBe('cabe45dcc9ae5b66ba86600cca6b8ba8')
    expect(md5Base64('')).toBe('1B2M2Y8AsgTpgAmY7PhCfg==')
  })

  it('computes CRC32 (IEEE)', () => {
    expect(crc32('123456789')).toBe(0xcbf43926)
    expect(crc32('')).toBe(0)
    // incremental
    expect(crc32('6789', crc32('12345'))).toBe(0xcbf43926)
    expect(crc32Base64('123456789')).toBe('y/Q5Jg==')
    expect(bytesToHex(new Uint8Array([0, 15, 255]))).toBe('000fff')
  })
})
