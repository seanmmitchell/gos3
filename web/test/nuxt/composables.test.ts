// Composable tests in the Nuxt runtime.
import { describe, expect, it } from 'vitest'
import { useConnectionsStore } from '~/stores/connections'
import { S3Error } from '~/utils/s3/errors'

describe('useAsyncAction', () => {
  it('tracks loading / error, toasts and never throws by default', async () => {
    const toast = useToast()
    const before = toast.toasts.value.length
    const ok = useAsyncAction(async (n: number) => n * 2, { successMessage: (n, r) => `${n} → ${r}` })
    await expect(ok.run(21)).resolves.toBe(42)
    expect(ok.loading.value).toBe(false)
    expect(toast.toasts.value.at(-1)?.title).toBe('21 → 42')

    const failing = useAsyncAction(async () => {
      throw new S3Error({ code: 'AccessDenied', message: 'Access Denied', status: 403 })
    }, { errorTitle: 'Could not do it' })
    await expect(failing.run()).resolves.toBeUndefined()
    expect(failing.error.value).toBeInstanceOf(S3Error)
    expect(toast.toasts.value.at(-1)?.title).toBe('Could not do it')
    expect(toast.toasts.value.length).toBe(before + 2)
  })

  it('does not toast aborted requests and can rethrow', async () => {
    const toast = useToast()
    const before = toast.toasts.value.length
    const aborted = useAsyncAction(async () => {
      throw new S3Error({ code: 'Aborted', message: 'aborted', status: 0 })
    }, { rethrow: true })
    await expect(aborted.run()).rejects.toBeInstanceOf(S3Error)
    expect(toast.toasts.value.length).toBe(before)
  })
})

describe('useConnection', () => {
  it('resolves memoised clients; admin only for gos3', () => {
    const store = useConnectionsStore()
    const g = store.add({ name: 'g', endpoint: 'http://127.0.0.1:9000', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 'gos3' })
    const s = store.add({ name: 's', endpoint: 'https://s3.example.com', accessKeyId: 'AK', secretAccessKey: 'SK', kind: 's3' })
    const n = store.add({ name: 'n', endpoint: 'http://10.0.0.9:9000', kind: 'gos3' })

    const a = useConnection(g.id)
    expect(a.connected.value).toBe(true)
    expect(a.s3.value).toBeDefined()
    expect(a.s3.value).toBe(useConnection(g.id).s3.value)
    expect(a.admin.value?.endpoint).toBe('http://127.0.0.1:9000')

    const b = useConnection(s.id)
    expect(b.s3.value).toBeDefined()
    expect(b.admin.value).toBeUndefined()
    expect(() => b.requireAdmin()).toThrow(/only available on gos3/)

    const c = useConnection(n.id)
    expect(c.connected.value).toBe(false)
    expect(() => c.requireS3()).toThrow(/not connected/)
  })
})
