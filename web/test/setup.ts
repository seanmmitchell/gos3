// Unit-test environment: Node's native fetch primitives (Request/Response/
// Headers, WebCrypto) — which behave like browsers — plus DOMParser from
// happy-dom and in-memory Web Storage. (happy-dom's own Headers keeps header
// name case, which breaks SigV4 canonicalisation, so we don't use its fetch.)
import { Window } from 'happy-dom'

const win = new Window()
;(globalThis as Record<string, unknown>).DOMParser = win.DOMParser

class MemoryStorage implements Storage {
  private map = new Map<string, string>()
  get length() {
    return this.map.size
  }

  clear() {
    this.map.clear()
  }

  getItem(key: string) {
    return this.map.has(key) ? this.map.get(key)! : null
  }

  key(i: number) {
    return [...this.map.keys()][i] ?? null
  }

  removeItem(key: string) {
    this.map.delete(key)
  }

  setItem(key: string, value: string) {
    this.map.set(key, String(value))
  }
}

Object.defineProperty(globalThis, 'localStorage', { value: new MemoryStorage(), configurable: true, writable: true })
Object.defineProperty(globalThis, 'sessionStorage', { value: new MemoryStorage(), configurable: true, writable: true })
