// Component tests in the Nuxt runtime (auto-imports, Nuxt UI) via
// @nuxt/test-utils. Use this file as the pattern for feature components.
import { mountSuspended } from '@nuxt/test-utils/runtime'
import { describe, expect, it } from 'vitest'
import { nextTick } from 'vue'
import ByteSize from '~/components/common/ByteSize.vue'
import EmptyState from '~/components/common/EmptyState.vue'
import ErrorAlert from '~/components/common/ErrorAlert.vue'
import JsonEditor from '~/components/common/JsonEditor.vue'
import KeyValueEditor from '~/components/common/KeyValueEditor.vue'
import PageHeader from '~/components/common/PageHeader.vue'
import StatusDot from '~/components/common/StatusDot.vue'
import type { KeyValuePair } from '~/types/ui'
import { S3Error } from '~/utils/s3/errors'

describe('common components', () => {
  it('PageHeader renders title, description and actions', async () => {
    const w = await mountSuspended(PageHeader, {
      props: { title: 'Buckets', description: 'All buckets', icon: 'i-lucide-database' },
      slots: { actions: () => 'ACTION' }
    })
    expect(w.find('h1').text()).toBe('Buckets')
    expect(w.text()).toContain('All buckets')
    expect(w.text()).toContain('ACTION')
  })

  it('EmptyState renders title and description with role=status', async () => {
    const w = await mountSuspended(EmptyState, { props: { title: 'Nothing here', description: 'Add something' } })
    expect(w.find('[role="status"]').exists()).toBe(true)
    expect(w.text()).toContain('Nothing here')
    expect(w.text()).toContain('Add something')
  })

  it('StatusDot exposes the state to screen readers', async () => {
    const w = await mountSuspended(StatusDot, { props: { state: 'unauthorized' } })
    expect(w.find('.sr-only').text()).toBe('Credentials rejected')
    const labelled = await mountSuspended(StatusDot, { props: { state: 'online', showLabel: true } })
    expect(labelled.text()).toContain('Online')
  })

  it('ByteSize formats sizes with the exact count as title', async () => {
    const w = await mountSuspended(ByteSize, { props: { bytes: 1536 } })
    expect(w.text()).toBe('1.5 KiB')
    expect(w.attributes('title')).toMatch(/1[,.\s ]?536 bytes/)
  })

  it('ErrorAlert describes S3 errors with code and request id', async () => {
    const err = new S3Error({ code: 'NoSuchBucket', message: 'The bucket does not exist', status: 404, requestId: 'REQ123' })
    const w = await mountSuspended(ErrorAlert, { props: { error: err } })
    expect(w.text()).toContain('NoSuchBucket (404)')
    expect(w.text()).toContain('The bucket does not exist')
    expect(w.text()).toContain('REQ123')
  })

  it('JsonEditor validates and formats JSON', async () => {
    const validity: boolean[] = []
    const w = await mountSuspended(JsonEditor, {
      props: {
        'modelValue': '{"a":1}',
        'label': 'Policy',
        'onUpdate:modelValue': (v: string) => w.setProps({ modelValue: v }),
        'onValidity': (v: boolean) => validity.push(v)
      }
    })
    expect(w.find('label').text()).toBe('Policy')
    expect(w.text()).toContain('Valid JSON')
    expect(validity.at(-1)).toBe(true)

    const format = w.findAll('button').find(b => b.text() === 'Format')!
    await format.trigger('click')
    expect(w.props('modelValue')).toBe('{\n  "a": 1\n}')

    await w.find('textarea').setValue('{"a":')
    await nextTick()
    expect(validity.at(-1)).toBe(false)
    expect(w.find('textarea').attributes('aria-invalid')).toBe('true')
  })

  it('KeyValueEditor flags duplicate keys and adds / removes rows', async () => {
    let pairs: KeyValuePair[] = [{ key: 'a', value: '1' }, { key: 'a', value: '2' }]
    const validity: boolean[] = []
    const w = await mountSuspended(KeyValueEditor, {
      props: {
        'modelValue': pairs,
        'onUpdate:modelValue': (v: KeyValuePair[]) => {
          pairs = v
          return w.setProps({ modelValue: v })
        },
        'onValidity': (v: boolean) => validity.push(v)
      }
    })
    expect(w.text()).toContain('Duplicate key')
    expect(validity.at(-1)).toBe(false)

    const remove = w.findAll('button').filter(b => b.attributes('aria-label')?.startsWith('Remove'))
    expect(remove).toHaveLength(2)
    await remove[1]!.trigger('click')
    expect(pairs).toEqual([{ key: 'a', value: '1' }])
    await nextTick()
    expect(validity.at(-1)).toBe(true)

    const add = w.findAll('button').find(b => b.text() === 'Add')!
    await add.trigger('click')
    expect(pairs).toHaveLength(2)
  })
})
