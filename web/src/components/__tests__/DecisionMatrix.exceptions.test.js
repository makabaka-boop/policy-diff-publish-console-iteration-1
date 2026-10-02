import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { nextTick } from 'vue'

// Mock the API boundary before importing the component under test.
const apiMocks = vi.hoisted(() => ({
  decisions: vi.fn(),
  listExceptions: vi.fn(),
  createException: vi.fn(),
}))
vi.mock('../../api.js', () => ({
  api: {
    decisions: apiMocks.decisions,
    listExceptions: apiMocks.listExceptions,
    createException: apiMocks.createException,
  },
}))

import DecisionMatrix from '../DecisionMatrix.vue'

const denyRow = (overrides = {}) => ({
  tuple: { role: 'viewer', resource: 'billing', action: 'read' },
  evidence: {
    decision: 'deny',
    reason: 'tie-deny',
    considered: [
      { ruleId: 'r-billing-read-deny', priority: 20, effect: 'deny' },
      { ruleId: 'r-billing-read-allow', priority: 20, effect: 'allow' },
    ],
    winners: [
      { ruleId: 'r-billing-read-allow', priority: 20, effect: 'allow' },
      { ruleId: 'r-billing-read-deny', priority: 20, effect: 'deny' },
    ],
  },
  ...overrides,
})

// A coherent override row: decision allow MUST be accompanied by both
// the exception identity and the preserved deny base evidence.
const exceptionRow = () =>
  denyRow({
    evidence: {
      decision: 'allow',
      reason: 'emergency-exception-allow',
      considered: [
        { ruleId: 'r-billing-read-deny', priority: 20, effect: 'deny' },
        { ruleId: 'r-billing-read-allow', priority: 20, effect: 'allow' },
      ],
      winners: [
        { ruleId: 'r-billing-read-allow', priority: 20, effect: 'allow' },
        { ruleId: 'r-billing-read-deny', priority: 20, effect: 'deny' },
      ],
      exception: {
        id: 'exc-p1-7',
        publishedRevision: 1,
        reason: 'drill break-glass',
        expiresAt: '2026-10-02T09:30:00Z',
      },
      baseEvidence: {
        decision: 'deny',
        reason: 'tie-deny',
        considered: [],
        winners: [
          { ruleId: 'r-billing-read-allow', priority: 20, effect: 'allow' },
          { ruleId: 'r-billing-read-deny', priority: 20, effect: 'deny' },
        ],
      },
    },
  })

function mountPublished(rows) {
  apiMocks.decisions.mockResolvedValue({ revision: 1, rows })
  return mount(DecisionMatrix, { props: { version: 'published', bump: 0 } })
}

beforeEach(() => {
  vi.clearAllMocks()
})

describe('DecisionMatrix with emergency exceptions', () => {
  it('shows a temporary allow cell with the exception marker', async () => {
    const wrapper = mountPublished([exceptionRow()])
    await flushPromises()

    const cell = wrapper.find('td.cell')
    expect(cell.classes()).toContain('cell-allow')
    expect(cell.find('.cell-exc').exists()).toBe(true)
  })

  it('never renders the mixed state: allow goes together with exception identity and base deny evidence', async () => {
    const wrapper = mountPublished([exceptionRow()])
    await flushPromises()

    // Click the cell to open the evidence box.
    await wrapper.find('td.cell').trigger('click')
    await nextTick()

    const box = wrapper.find('.evidence-box')
    expect(box.exists()).toBe(true)

    // Effective verdict is ALLOW...
    expect(box.text()).toContain('ALLOW')
    expect(box.text()).toContain('模拟应急例外临时放行')

    // ...the exception identity is visible...
    expect(box.text()).toContain('exc-p1-7')
    expect(box.text()).toContain('p1')
    expect(box.text()).toContain('drill break-glass')

    // ...and the original published DENY ruling + its rule evidence stay
    // on the same page, so nobody can mistake this for a pure rule allow.
    const base = box.find('.base-ruling')
    expect(base.exists()).toBe(true)
    expect(base.text()).toContain('DENY')
    expect(box.text()).toContain('r-billing-read-deny')
    expect(box.text()).toContain('即原发布规则证据')
  })

  it('renders a plain deny row with no exception chrome', async () => {
    const wrapper = mountPublished([denyRow()])
    await flushPromises()

    const cell = wrapper.find('td.cell')
    expect(cell.classes()).toContain('cell-deny')
    expect(cell.find('.cell-exc').exists()).toBe(false)

    await cell.trigger('click')
    await nextTick()
    const box = wrapper.find('.evidence-box')
    expect(box.text()).toContain('DENY')
    expect(box.find('.exception-evidence').exists()).toBe(false)
    expect(box.text()).toContain('同优先级冲突，deny 胜出')
  })

  it('refetches when the bump changes so publish/expiry re-render atomically', async () => {
    const wrapper = mountPublished([exceptionRow()])
    await flushPromises()
    expect(apiMocks.decisions).toHaveBeenCalledTimes(1)

    // After expiry the server returns the pure deny row.
    apiMocks.decisions.mockResolvedValue({ revision: 1, rows: [denyRow()] })
    await wrapper.setProps({ bump: 1 })
    await flushPromises()

    expect(apiMocks.decisions).toHaveBeenCalledTimes(2)
    const cell = wrapper.find('td.cell')
    expect(cell.classes()).toContain('cell-deny')
    expect(cell.find('.cell-exc').exists()).toBe(false)
  })
})
