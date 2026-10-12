import { describe, expect, it } from 'vitest'
import {
  collectDroppedFiles,
  dragHasFiles,
  filesFromInput,
  flattenEntries,
  readAllEntries
} from '~/composables/useBrowserDrop'
import type { DataTransferItemLike, FsDirectoryEntryLike, FsEntryLike, FsFileEntryLike } from '~/composables/useBrowserDrop'

// Fakes of the FileSystemEntry API. Directory readers return their children
// in batches of `batch` (Chrome returns at most 100 per readEntries call).
function fileEntry(name: string, content = name, fail = false): FsFileEntryLike {
  return {
    isFile: true,
    isDirectory: false,
    name,
    file(ok, err) {
      setTimeout(() => (fail ? err?.(new Error('permission denied')) : ok(new File([content], name))), 0)
    }
  }
}

function dirEntry(name: string, children: FsEntryLike[], batch = 2): FsDirectoryEntryLike {
  return {
    isFile: false,
    isDirectory: true,
    name,
    createReader() {
      let pos = 0
      return {
        readEntries(ok) {
          const out = children.slice(pos, pos + batch)
          pos += batch
          setTimeout(() => ok(out), 0)
        }
      }
    }
  }
}

function item(entry: FsEntryLike | null, file: File | null = null): DataTransferItemLike {
  return { kind: 'file', getAsFile: () => file, webkitGetAsEntry: () => entry }
}

describe('folder drag & drop flattening', () => {
  it('reads directories until readEntries returns an empty batch', async () => {
    const d = dirEntry('d', ['1', '2', '3', '4', '5'].map(n => fileEntry(n)), 2)
    expect((await readAllEntries(d)).map(e => e.name)).toEqual(['1', '2', '3', '4', '5'])
  })

  it('flattens nested folders preserving relative paths', async () => {
    const tree = [
      dirEntry('photos', [
        fileEntry('a.jpg'),
        dirEntry('2024', [fileEntry('b.jpg'), fileEntry('c.jpg'), dirEntry('empty', [])]),
        fileEntry('d.jpg')
      ]),
      fileEntry('top.txt', 'hello')
    ]
    const r = await flattenEntries(tree)
    expect(r.errors).toEqual([])
    expect(r.truncated).toBe(false)
    expect(r.files.map(f => f.relativePath)).toEqual(['photos/a.jpg', 'photos/2024/b.jpg', 'photos/2024/c.jpg', 'photos/d.jpg', 'top.txt'])
    expect(await r.files[4]!.file.text()).toBe('hello')
  })

  it('reports unreadable files and stops at maxFiles', async () => {
    const r = await flattenEntries([dirEntry('x', [fileEntry('ok'), fileEntry('bad', '', true), fileEntry('ok2'), fileEntry('ok3')])], { maxFiles: 2 })
    expect(r.files.map(f => f.relativePath)).toEqual(['x/ok', 'x/ok2'])
    expect(r.errors).toEqual(['x/bad: permission denied'])
    expect(r.truncated).toBe(true)
  })

  it('collects from a DataTransfer with entries, mixing folders and files', async () => {
    const plain = new File(['p'], 'plain.bin')
    const dt = {
      items: [item(dirEntry('docs', [fileEntry('r.md')])), item(null, plain), { kind: 'string', getAsFile: () => null }],
      files: []
    }
    const r = await collectDroppedFiles(dt)
    expect(r.files.map(f => f.relativePath)).toEqual(['docs/r.md', 'plain.bin'])
  })

  it('falls back to DataTransfer.files without the entries API', async () => {
    const a = new File(['a'], 'a.txt')
    const r = await collectDroppedFiles({ items: null, files: [a] })
    expect(r.files).toEqual([{ file: a, relativePath: 'a.txt' }])
    expect((await collectDroppedFiles(null)).files).toEqual([])
  })

  it('keeps webkitRelativePath for folder picks', () => {
    const f = new File(['x'], 'b.txt')
    Object.defineProperty(f, 'webkitRelativePath', { value: 'folder/sub/b.txt' })
    const g = new File(['y'], 'c.txt')
    expect(filesFromInput([f, g]).map(x => x.relativePath)).toEqual(['folder/sub/b.txt', 'c.txt'])
    expect(filesFromInput(null)).toEqual([])
  })

  it('detects file drags', () => {
    expect(dragHasFiles({ types: ['Files', 'text/plain'] })).toBe(true)
    expect(dragHasFiles({ types: ['text/uri-list'] })).toBe(false)
    expect(dragHasFiles(null)).toBe(false)
  })
})
