// Component tests for the admin pages and bucket settings building blocks.
import { mountSuspended } from '@nuxt/test-utils/runtime'
import { describe, expect, it } from 'vitest'
import { nextTick } from 'vue'
import AccessKeysTable from '~/components/admin/AccessKeysTable.vue'
import DiskUsage from '~/components/admin/DiskUsage.vue'
import OpsTable from '~/components/admin/OpsTable.vue'
import StatCard from '~/components/admin/StatCard.vue'
import TrafficChart from '~/components/admin/TrafficChart.vue'
import CorsRuleEditor from '~/components/bucket-settings/CorsRuleEditor.vue'
import { newCorsRule, type CorsRuleForm } from '~/components/bucket-settings/-cors'
import { fillMinutes } from '~/components/admin/-metrics'
import type { AccessKey, OpStats } from '~/types/admin'

const op = (name: string, requests: number, p99: number): OpStats => ({
  op: name, requests, errors4xx: 0, errors5xx: 0, bytesIn: 0, bytesOut: 0, avgLatencyMs: 1, p50LatencyMs: 1, p99LatencyMs: p99
})

describe('admin components', () => {
  it('StatCard renders label, value and hint', async () => {
    const w = await mountSuspended(StatCard, { props: { label: 'Requests', value: '1,234', hint: 'since start' } })
    expect(w.find('dt').text()).toBe('Requests')
    expect(w.text()).toContain('1,234')
    expect(w.text()).toContain('since start')
  })

  it('OpsTable sorts by clicking column headers and exposes aria-sort', async () => {
    const w = await mountSuspended(OpsTable, { props: { ops: [op('PutObject', 5, 30), op('GetObject', 50, 2), op('ListObjectsV2', 10, 9)] } })
    const names = () => w.findAll('tbody th').map(t => t.text())
    expect(names()).toEqual(['GetObject', 'ListObjectsV2', 'PutObject'])
    const headers = w.findAll('thead th')
    expect(headers[1]!.attributes('aria-sort')).toBe('descending')

    await headers[8]!.find('button').trigger('click') // p99
    expect(names()).toEqual(['PutObject', 'ListObjectsV2', 'GetObject'])
    expect(w.findAll('thead th')[8]!.attributes('aria-sort')).toBe('descending')
    expect(w.findAll('thead th')[1]!.attributes('aria-sort')).toBe('none')

    await w.findAll('thead th')[8]!.find('button').trigger('click')
    expect(names()).toEqual(['GetObject', 'ListObjectsV2', 'PutObject'])
    expect(w.findAll('thead th')[8]!.attributes('aria-sort')).toBe('ascending')
  })

  it('OpsTable shows an empty row without data', async () => {
    const w = await mountSuspended(OpsTable, { props: { ops: null } })
    expect(w.text()).toContain('No requests recorded yet.')
  })

  it('TrafficChart draws one line + area per series and summarises for screen readers', async () => {
    const recent = [
      { minute: '2026-10-09T10:00:00Z', requests: 10, errors: 1, bytesIn: 0, bytesOut: 0 },
      { minute: '2026-10-09T10:01:00Z', requests: 30, errors: 0, bytesIn: 0, bytesOut: 0 }
    ]
    const w = await mountSuspended(TrafficChart, {
      props: {
        title: 'Requests per minute',
        points: fillMinutes(recent),
        series: [
          { key: 'requests', label: 'Requests', color: 'primary' },
          { key: 'errors', label: 'Errors', color: 'error' }
        ]
      }
    })
    const svg = w.find('svg[role="img"]')
    expect(svg.exists()).toBe(true)
    expect(svg.attributes('aria-label')).toContain('Requests: 40 total, peak 30 per minute')
    expect(svg.attributes('aria-label')).toContain('Errors: 1 total')
    const paths = w.findAll('path')
    expect(paths).toHaveLength(4)
    expect(paths.every(p => (p.attributes('d') ?? '').startsWith('M'))).toBe(true)
    expect(w.text()).toContain('now')

    // Keyboard inspection announces the selected minute.
    await svg.trigger('keydown', { key: 'End' })
    expect(w.find('[aria-live="polite"]').text()).toContain('Requests 30')
  })

  it('DiskUsage exposes a meter with the used percentage', async () => {
    const w = await mountSuspended(DiskUsage, { props: { disk: { path: '/data', totalBytes: 1000, usedBytes: 250, freeBytes: 750 } } })
    const meter = w.find('[role="meter"]')
    expect(meter.attributes('aria-valuenow')).toBe('25')
    expect(meter.attributes('aria-valuetext')).toContain('25.0%')
    expect(w.text()).toContain('/data')
  })

  it('AccessKeysTable lists keys and protects the key of this session', async () => {
    const keys: AccessKey[] = [
      { accessKey: 'AKIACURRENT', status: 'Active', createdAt: '2026-01-01T00:00:00Z', description: 'laptop' },
      { accessKey: 'AKIAOTHER', status: 'Inactive', createdAt: '2026-02-01T00:00:00Z' }
    ]
    const w = await mountSuspended(AccessKeysTable, { props: { keys, currentAccessKey: 'AKIACURRENT' } })
    expect(w.text()).toContain('AKIACURRENT')
    expect(w.text()).toContain('this session')
    expect(w.text()).toContain('laptop')
    expect(w.text()).toContain('Inactive')
    const del = w.find('button[aria-label="Delete access key AKIACURRENT"]')
    expect(del.attributes('disabled')).toBeDefined()
    await w.find('button[aria-label="Delete access key AKIAOTHER"]').trigger('click')
    expect(w.emitted('delete')?.[0]?.[0]).toMatchObject({ accessKey: 'AKIAOTHER' })
    const activate = w.findAll('button').find(b => b.text() === 'Activate')
    await activate!.trigger('click')
    expect(w.emitted('toggle')?.[0]?.[0]).toMatchObject({ accessKey: 'AKIAOTHER' })
  })

  it('AccessKeysTable hides activate/deactivate in self-service mode', async () => {
    const w = await mountSuspended(AccessKeysTable, { props: { keys: [{ accessKey: 'AK1', status: 'Active', createdAt: '2026-01-01T00:00:00Z' }], canToggle: false } })
    expect(w.findAll('button').some(b => b.text() === 'Deactivate')).toBe(false)
  })
})

describe('bucket settings components', () => {
  it('CorsRuleEditor toggles methods and edits fields immutably', async () => {
    const updates: CorsRuleForm[] = []
    const rule = newCorsRule()
    const w = await mountSuspended(CorsRuleEditor, {
      props: {
        'modelValue': rule,
        'index': 0,
        'onUpdate:modelValue': (v: CorsRuleForm) => updates.push(v)
      }
    })
    expect(w.text()).toContain('Rule 1')
    expect(w.find('fieldset legend').text()).toBe('Allowed methods')
    const checkboxIds = w.findAll('button[role="checkbox"]').map(b => b.attributes('id'))
    expect(new Set(checkboxIds).size).toBe(checkboxIds.length)
    const label = w.findAll('label').find(l => l.text() === 'PUT')
    const put = w.find(`[id="${label!.attributes('for')}"]`)
    expect(put.attributes('role')).toBe('checkbox')
    await put.trigger('click')
    await nextTick()
    expect(updates.at(-1)?.allowedMethods).toEqual(['GET', 'PUT', 'HEAD'])
    expect(rule.allowedMethods).toEqual(['GET', 'HEAD'])

    const remove = w.findAll('button').find(b => b.text() === 'Remove rule 1')
    await remove!.trigger('click')
    expect(w.emitted('remove')).toHaveLength(1)
  })

  it('CorsRuleEditor shows validation errors', async () => {
    const w = await mountSuspended(CorsRuleEditor, {
      props: { modelValue: { ...newCorsRule(), allowedOrigins: [] }, index: 2, errors: { allowedOrigins: 'At least one origin is required' } }
    })
    expect(w.text()).toContain('Rule 3')
    expect(w.text()).toContain('At least one origin is required')
  })
})
