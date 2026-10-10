// Review fixes in object-browser components: stale responses must not
// overwrite newer ones; relative timestamps share one timer.
import { mountSuspended } from '@nuxt/test-utils/runtime'
import { flushPromises } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import { h } from 'vue'
import ObjectTagsEditor from '~/components/browser/ObjectTagsEditor.vue'
import ObjectVersions from '~/components/browser/ObjectVersions.vue'
import RelativeTime from '~/components/common/RelativeTime.vue'
import type { ObjectVersion, Tag } from '~/types/s3'
import type { S3Client } from '~/utils/s3/client'

function deferred<T>() {
  let resolve!: (v: T) => void
  const promise = new Promise<T>(r => (resolve = r))
  return { promise, resolve }
}

describe('stale detail responses', () => {
  it('ObjectTagsEditor keeps the tags of the version shown last', async () => {
    const pending = new Map<string, ReturnType<typeof deferred<Tag[]>>>()
    const client = {
      getObjectTagging: (_b: string, _k: string, v?: string) => {
        const d = deferred<Tag[]>()
        pending.set(v ?? 'latest', d)
        return d.promise
      }
    } as unknown as S3Client
    const w = await mountSuspended(ObjectTagsEditor, { props: { client, bucket: 'b', objectKey: 'k', versionId: 'old' } })
    await w.setProps({ versionId: undefined })
    pending.get('latest')!.resolve([{ key: 'env', value: 'latest' }])
    await flushPromises()
    pending.get('old')!.resolve([{ key: 'env', value: 'old' }])
    await flushPromises()
    const values = w.findAll('input').map(i => (i.element as HTMLInputElement).value)
    expect(values).toContain('latest')
    expect(values).not.toContain('old')
  })

  it('ObjectVersions keeps the versions of the key shown last', async () => {
    const pending = new Map<string, ReturnType<typeof deferred<ObjectVersion[]>>>()
    const client = {
      listVersionsOfKey: (_b: string, k: string) => {
        const d = deferred<ObjectVersion[]>()
        pending.set(k, d)
        return d.promise
      }
    } as unknown as S3Client
    const v = (key: string, versionId: string): ObjectVersion => ({ key, versionId, isLatest: true, size: 1, isDeleteMarker: false })
    const w = await mountSuspended(ObjectVersions, { props: { client, bucket: 'b', objectKey: 'first' } })
    await w.setProps({ objectKey: 'second' })
    pending.get('second')!.resolve([v('second', 'v-second')])
    await flushPromises()
    pending.get('first')!.resolve([v('first', 'v-first')])
    await flushPromises()
    expect(w.text()).toContain('v-second')
    expect(w.text()).not.toContain('v-first')
  })
})

describe('CommonRelativeTime', () => {
  it('uses one shared interval for any number of instances', async () => {
    const spy = vi.spyOn(globalThis, 'setInterval')
    const before = spy.mock.calls.filter(c => c[1] === 30000).length
    const w = await mountSuspended({
      render: () => h('div', Array.from({ length: 50 }, (_, i) => h(RelativeTime, { value: Date.now() - i * 60000 })))
    })
    expect(w.findAll('time')).toHaveLength(50)
    expect(spy.mock.calls.filter(c => c[1] === 30000).length - before).toBeLessThanOrEqual(1)
    w.unmount()
  })
})
