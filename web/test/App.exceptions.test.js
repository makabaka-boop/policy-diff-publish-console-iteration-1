import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import App from '../src/App.vue'

const doc = {
  roles: [{ name: 'base', parents: [] }],
  resources: ['doc'],
  actions: ['read'],
  rules: [{ id: 'base-deny', role: '*', resource: '*', action: '*', priority: 0, effect: 'deny' }],
}

const denyRow = {
  tuple: { role: 'base', resource: 'doc', action: 'read' },
  evidence: {
    decision: 'deny', reason: 'single-winner',
    considered: [{ ruleId: 'base-deny', priority: 0, effect: 'deny' }],
    winners: [{ ruleId: 'base-deny', priority: 0, effect: 'deny' }],
  },
}
const exRow = (id = 'ex-1', rev = 1) => ({
  tuple: denyRow.tuple,
  evidence: {
    ...denyRow.evidence,
    decision: 'allow', reason: 'emergency-exception-allow',
    exceptionId: id, originalReason: 'single-winner',
  },
  exception: {
    id, tuple: denyRow.tuple, reason: 'drill',
    createdAt: '2026-10-02T08:00:00Z', expiresAt: '2026-10-02T08:10:00Z',
    publishedRevision: rev,
  },
})
const ex = (id = 'ex-1') => exRow(id).exception

function res(status, body) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

// Route table, mutable per test.
let routes = {}

function installFetch() {
  global.fetch = vi.fn(async (url, opts = {}) => {
    const u = String(url).replace(/^https?:\/\/[^/]+/, '')
    const method = opts.method || 'GET'
    const key = `${method} ${u}`
    const h = routes[key]
    if (h) return h(opts)
    return res(404, { error: 'unmocked ' + key })
  })
}

function standardRoutes({ pubRev = 1, draftRev = 1, rows = [denyRow], exceptions = [] } = {}) {
  routes = {
    'GET /api/state': () => res(200, {
      draft: { document: doc, revision: draftRev },
      published: { document: doc, revision: pubRev },
      notice: 'SIM',
    }),
    'GET /api/exceptions': () => res(200, {
      exceptions, publishedRevision: pubRev, now: '2026-10-02T08:00:00Z',
    }),
    'GET /api/decisions/draft': () => res(200, {
      version: 'draft', revision: draftRev, rows: [denyRow],
    }),
    'GET /api/decisions/published': () => res(200, {
      version: 'published', revision: pubRev, rows,
    }),
    'POST /api/preview': () => res(200, {
      summary: {
        draftRevision: draftRev, publishedRevision: pubRev, tupleCount: 1,
        allowCount: 0, denyCount: 1, newAllows: [], newDenies: [],
        unchanged: 1, hash: 'h',
      },
      draftRevision: draftRev, publishedRevision: pubRev,
    }),
  }
}

function publishedTab(w) {
  const tabs = w.findAll('.tab')
  return tabs.find((t) => t.text().includes('已发布'))
}

beforeEach(() => { vi.useRealTimers(); routes = {} })
afterEach(() => { vi.useRealTimers() })

describe('App — exception panel + matrix coherence', () => {
  it('shows active exceptions in the panel and renders the published cell as exception allow', async () => {
    installFetch()
    standardRoutes({ rows: [exRow()], exceptions: [ex()] })
    const w = mount(App)
    await flushPromises()
    await publishedTab(w).trigger('click')
    await flushPromises()

    expect(w.find('.exceptions-card').text()).toContain('ex-1')
    expect(w.find('.exceptions-card').text()).toContain('drill')
    const cell = w.find('tbody td.cell')
    expect(cell.classes()).toContain('cell-exception')
    // Chip counter visible.
    expect(w.find('.ex-chip').text()).toContain('1')
  })

  it('publishing a new policy immediately invalidates old exceptions', async () => {
    installFetch()
    // Start: draft r2, published p1 with an active p1 exception.
    standardRoutes({ pubRev: 1, draftRev: 2, rows: [exRow()], exceptions: [ex()] })
    const w = mount(App)
    await flushPromises()
    await publishedTab(w).trigger('click')
    await flushPromises()
    expect(w.find('tbody td.cell').classes()).toContain('cell-exception')

    // Publish succeeds; after that all reads see p2 with no exceptions.
    let publishCalls = 0
    routes['POST /api/publish'] = () => {
      publishCalls++
      return res(200, {
        published: { document: doc, revision: 2 },
        summary: { hash: 'h2' },
      })
    }
    routes['GET /api/state'] = () => res(200, {
      draft: { document: doc, revision: 2 },
      published: { document: doc, revision: 2 },
      notice: 'SIM',
    })
    routes['GET /api/exceptions'] = () => res(200, {
      exceptions: [], publishedRevision: 2, now: '2026-10-02T08:00:00Z',
    })
    routes['GET /api/decisions/published'] = () => res(200, {
      version: 'published', revision: 2, rows: [denyRow],
    })

    // Generate a preview over the live (r2/p1) pair, then publish.
    await w.find('.preview-card button').trigger('click')
    await flushPromises()
    const pubBtn = w.find('.publish-row button')
    expect(pubBtn.attributes('disabled')).toBeUndefined()
    await pubBtn.trigger('click')
    await flushPromises()
    await flushPromises()

    expect(publishCalls).toBe(1)
    // Matrix switched to published p2 and shows plain deny again.
    const cell = w.find('tbody td.cell')
    expect(cell.classes()).toContain('cell-deny')
    expect(cell.classes()).not.toContain('cell-exception')
    expect(w.find('.exceptions-card').text()).toContain('没有生效中')
  })

  it('reset immediately clears exceptions', async () => {
    installFetch()
    standardRoutes({ rows: [exRow()], exceptions: [ex()] })
    const w = mount(App)
    await flushPromises()
    expect(w.find('.ex-chip').exists()).toBe(true)

    routes['POST /api/demo/reset'] = () => res(200, {
      draft: { document: doc, revision: 1 },
      published: { document: doc, revision: 1 },
      notice: 'SIM',
    })
    // Post-reset reads: empty exception list.
    routes['GET /api/exceptions'] = () => res(200, {
      exceptions: [], publishedRevision: 1, now: '2026-10-02T08:00:00Z',
    })
    routes['GET /api/decisions/published'] = () => res(200, {
      version: 'published', revision: 1, rows: [denyRow],
    })

    const buttons = w.findAll('button')
    await buttons.find((b) => b.text().includes('重置')).trigger('click')
    await flushPromises()
    await flushPromises()

    expect(w.find('.ex-chip').exists()).toBe(false)
    expect(w.find('.exceptions-card').text()).toContain('没有生效中')
  })

  it('countdown reaches expired state and a refresh restores the original verdict (lazy expiry)', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-10-02T08:00:00Z'))
    installFetch()
    standardRoutes({ rows: [exRow()], exceptions: [ex()] })
    const w = mount(App)
    await flushPromises()

    expect(w.find('.exceptions-card').text()).toContain('10 分') // 08:00 -> 08:10

    // Advance the local clock past expiry: the UI reports expired
    // while the server has not yet been read (lazy expiry, no job).
    vi.advanceTimersByTime(11 * 60 * 1000)
    await flushPromises()
    expect(w.find('.exceptions-card').text()).toContain('已到期')

    // Next authoritative 5s refetch finds the server lazily purged it,
    // and the published row reverts to deny.
    routes['GET /api/exceptions'] = () => res(200, {
      exceptions: [], publishedRevision: 1, now: '2026-10-02T08:11:00Z',
    })
    routes['GET /api/decisions/published'] = () => res(200, {
      version: 'published', revision: 1, rows: [denyRow],
    })
    await publishedTab(w).trigger('click')
    await flushPromises()
    // Fire only one scheduled refresh tick (the 5s poll), not every
    // queued timer, to avoid walking the recurring interval forever.
    vi.advanceTimersByTime(5 * 1000)
    await flushPromises()

    const cell = w.find('tbody td.cell')
    expect(cell.classes()).toContain('cell-deny')
    expect(w.find('.ex-chip').exists()).toBe(false)
  })

  it('a create carrying an old publishedRevision is rejected on publish race (interleaved)', async () => {
    installFetch()
    // Page shows p1.
    standardRoutes({ pubRev: 1, rows: [denyRow], exceptions: [] })
    const w = mount(App)
    await flushPromises()
    await publishedTab(w).trigger('click')
    await flushPromises()

    // Another client publishes concurrently: exceptions create for p1
    // now returns published_moved.
    routes['POST /api/exceptions'] = () => res(409, {
      error: 'moved', code: 'published_moved',
    })
    const cell = w.find('tbody td.cell')
    await cell.trigger('click')
    await w.find('.exception-form textarea').setValue('late request')
    await w.find('.exception-form input[type=number]').setValue(5)
    await w.find('.exception-form button').trigger('click')
    await flushPromises()
    expect(w.find('.exception-form .banner-err').text()).toContain('修订')
    // No chip, cell still deny — the late create never attached.
    expect(w.find('.ex-chip').exists()).toBe(false)
    expect(w.find('tbody td.cell').classes()).toContain('cell-deny')
  })
})
