import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import DecisionMatrix from '../src/components/DecisionMatrix.vue'

// ---- fixtures ----
function denyRow(extra = {}) {
  return {
    tuple: { role: 'base', resource: 'doc', action: 'read' },
    evidence: {
      decision: 'deny',
      reason: 'single-winner',
      considered: [{ ruleId: 'base-deny', priority: 0, effect: 'deny' }],
      winners: [{ ruleId: 'base-deny', priority: 0, effect: 'deny' }],
    },
    ...extra,
  }
}
function exceptionRow() {
  const r = denyRow({ tuple: { role: 'base', resource: 'doc', action: 'write' } })
  r.evidence = {
    ...r.evidence,
    decision: 'allow',
    reason: 'emergency-exception-allow',
    exceptionId: 'ex-1',
    originalReason: 'single-winner',
  }
  r.exception = {
    id: 'ex-1',
    tuple: r.tuple,
    reason: '演练放行',
    createdAt: '2026-10-02T08:00:00Z',
    expiresAt: '2026-10-02T08:05:00Z',
    publishedRevision: 1,
  }
  return r
}

function jsonRes(status, body) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

function mountMatrix(rows, { version = 'published', rev = 1, props = {} } = {}) {
  return mount(DecisionMatrix, {
    props: {
      version,
      bump: 0,
      publishedRevision: 1,
      serverNow: '2026-10-02T08:00:00Z',
      nowTick: 0,
      ...props,
    },
    global: {},
    // attach a per-instance decisions payload through the fetch mock
  })
}

let postedBodies = []
let decisionsPayload = null
let postOverride = null

function installFetch() {
  postedBodies = []
  global.fetch = vi.fn(async (url, opts = {}) => {
    const u = String(url)
    const method = opts.method || 'GET'
    if (method === 'GET' && u.includes('/api/decisions/')) {
      return jsonRes(200, decisionsPayload)
    }
    if (method === 'POST' && u.endsWith('/api/exceptions')) {
      postedBodies.push(JSON.parse(opts.body))
      if (postOverride) return postOverride()
      return jsonRes(201, { exception: exceptionRow().exception, decision: exceptionRow() })
    }
    return jsonRes(404, { error: 'unmocked ' + u })
  })
}

function cells(wrapper) {
  return wrapper.findAll('tbody td.cell')
}

beforeEach(() => {
  decisionsPayload = { version: 'published', revision: 1, rows: [] }
  postOverride = null
})

describe('DecisionMatrix — emergency exception page semantics', () => {
  it('renders exception allow and deny coherently: same cell carries id, original deny evidence', async () => {
    installFetch()
    decisionsPayload.rows = [denyRow(), exceptionRow()]
    const w = mountMatrix(decisionsPayload.rows)
    await flushPromises()

    const cs = cells(w)
    expect(cs.length).toBe(2)
    expect(cs[0].classes()).toContain('cell-deny')
    expect(cs[1].classes()).toContain('cell-allow')
    expect(cs[1].classes()).toContain('cell-exception')

    // Open the exception cell: allow + exception identity + ORIGINAL
    // deny rule evidence appear together — never a mixed display.
    await cs[1].trigger('click')
    const box = w.find('.evidence-box')
    expect(box.exists()).toBe(true)
    expect(box.find('.exception-block').exists()).toBe(true)
    expect(box.text()).toContain('ex-1')
    expect(box.text()).toContain('base-deny')
    expect(box.text()).toContain('应急')
    expect(box.find('.ex-tag').text()).toContain('ex-1')
    expect(box.text()).toContain('ALLOW')
  })

  it('offers the create form only on a published deny tuple and posts a re-adjudication request', async () => {
    installFetch()
    decisionsPayload.rows = [denyRow()]
    const w = mountMatrix(decisionsPayload.rows)
    await flushPromises()

    await cells(w)[0].trigger('click')
    const form = w.find('.exception-form')
    expect(form.exists()).toBe(true)
    expect(form.text()).toContain('重新裁决')

    await form.find('textarea').setValue('演练：临时读 5 分钟')
    await form.find('input[type=number]').setValue(5)
    await form.find('button').trigger('click')
    await flushPromises()

    expect(postedBodies).toHaveLength(1)
    expect(postedBodies[0]).toMatchObject({
      tuple: { role: 'base', resource: 'doc', action: 'read' },
      reason: '演练：临时读 5 分钟',
      ttlMinutes: 5,
      // Pinned to the revision whose evidence is on screen.
      publishedRevision: 1,
    })
  })

  it('after successful create the cell flips to exception allow and emits exception-changed', async () => {
    installFetch()
    decisionsPayload.rows = [denyRow()]
    const w = mountMatrix()
    await flushPromises()
    await cells(w)[0].trigger('click')
    const form = w.find('.exception-form')

    // Second decisions fetch returns the override for the same tuple.
    const overridden = (() => {
      const r = denyRow()
      r.evidence = {
        ...r.evidence,
        decision: 'allow',
        reason: 'emergency-exception-allow',
        exceptionId: 'ex-9',
        originalReason: 'single-winner',
      }
      r.exception = {
        id: 'ex-9', tuple: r.tuple, reason: 'r',
        createdAt: '2026-10-02T08:00:00Z', expiresAt: '2026-10-02T08:05:00Z',
        publishedRevision: 1,
      }
      return r
    })()
    let calls = 0
    global.fetch = vi.fn(async (url, opts = {}) => {
      const u = String(url)
      if ((opts.method || 'GET') === 'GET' && u.includes('/api/decisions/')) {
        calls++
        return jsonRes(200, {
          version: 'published', revision: 1,
          rows: [calls === 1 ? denyRow() : overridden],
        })
      }
      if ((opts.method || 'POST') === 'POST' && u.endsWith('/api/exceptions')) {
        return jsonRes(201, { exception: overridden.exception, decision: overridden })
      }
      return jsonRes(404, { error: 'x' })
    })
    const w2 = mountMatrix()
    await flushPromises()
    await cells(w2)[0].trigger('click')
    await w2.find('.exception-form textarea').setValue('reason')
    await w2.find('.exception-form input[type=number]').setValue(10)
    await w2.find('.exception-form button').trigger('click')
    await flushPromises()
    await flushPromises()

    expect(cells(w2)[0].classes()).toContain('cell-exception')
    expect(w2.emitted('exception-changed')).toBeTruthy()
    // The evidence view reopened on the new override.
    const box = w2.find('.evidence-box')
    expect(box.find('.exception-block').exists()).toBe(true)
    expect(box.text()).toContain('ex-9')
  })

  it('rejects missing reason and out-of-range TTL locally: whole attempt, no request', async () => {
    installFetch()
    decisionsPayload.rows = [denyRow()]
    const w = mountMatrix()
    await flushPromises()
    await cells(w)[0].trigger('click')

    const submit = async () => {
      await w.find('.exception-form button').trigger('click')
      await flushPromises()
    }
    // Empty reason.
    await w.find('.exception-form input[type=number]').setValue(5)
    await submit()
    expect(w.find('.exception-form .banner-err').text()).toContain('理由')
    expect(postedBodies).toHaveLength(0)

    // Boundary: 0 and 61 blocked.
    await w.find('.exception-form textarea').setValue('ok')
    await w.find('.exception-form input[type=number]').setValue(0)
    await submit()
    expect(postedBodies).toHaveLength(0)
    await w.find('.exception-form input[type=number]').setValue(61)
    await submit()
    expect(postedBodies).toHaveLength(0)

    // Boundary: 1 and 60 are accepted (request sent).
    for (const ttl of [1, 60]) {
      await w.find('.exception-form input[type=number]').setValue(ttl)
      await submit()
    }
    expect(postedBodies.map((b) => b.ttlMinutes)).toEqual([1, 60])
  })

  it('surfaces published_moved so a late request cannot bind to a new revision', async () => {
    installFetch()
    postOverride = () => jsonRes(409, {
      error: 'published revision moved', code: 'published_moved',
    })
    decisionsPayload.rows = [denyRow()]
    const w = mountMatrix()
    await flushPromises()
    await cells(w)[0].trigger('click')
    await w.find('.exception-form textarea').setValue('late')
    await w.find('.exception-form input[type=number]').setValue(5)
    await w.find('.exception-form button').trigger('click')
    await flushPromises()

    expect(w.find('.exception-form .banner-err').text()).toContain('修订')
    // Cell stays deny: no optimistic release on failure.
    expect(cells(w)[0].classes()).toContain('cell-deny')
  })

  it('surfaces tuple_not_denied after server re-adjudication', async () => {
    installFetch()
    postOverride = () => jsonRes(409, {
      error: 'currently allowed', code: 'tuple_not_denied',
    })
    decisionsPayload.rows = [denyRow()]
    const w = mountMatrix()
    await flushPromises()
    await cells(w)[0].trigger('click')
    await w.find('.exception-form textarea').setValue('x')
    await w.find('.exception-form input[type=number]').setValue(5)
    await w.find('.exception-form button').trigger('click')
    await flushPromises()
    expect(w.find('.exception-form .banner-err').text()).toContain('并非 deny')
  })

  it('never offers the form on draft decisions', async () => {
    installFetch()
    decisionsPayload = { version: 'draft', revision: 2, rows: [denyRow()] }
    const w = mount(DecisionMatrix, {
      props: { version: 'draft', bump: 0, publishedRevision: 1, serverNow: '', nowTick: 0 },
    })
    await flushPromises()
    await cells(w)[0].trigger('click')
    expect(w.find('.exception-form').exists()).toBe(false)
    expect(w.text()).not.toContain('应急')
  })

  it('restores the original verdict after expiry on refetch (no background task)', async () => {
    let calls = 0
    global.fetch = vi.fn(async () => {
      calls++
      const rows = calls === 1 ? [exceptionRow()] : [denyRow({
        tuple: { role: 'base', resource: 'doc', action: 'write' },
      })]
      return jsonRes(200, { version: 'published', revision: 1, rows })
    })
    const w = mount(DecisionMatrix, {
      props: {
        version: 'published', bump: 0, publishedRevision: 1,
        serverNow: '2026-10-02T08:00:00Z', nowTick: 0,
      },
    })
    await flushPromises()
    expect(cells(w)[0].classes()).toContain('cell-exception')
    await cells(w)[0].trigger('click')
    expect(w.find('.evidence-box').text()).toMatch(/剩余\s*\d+\s*分/) // countdown present

    // Simulate server-side lazy expiry: parent bumps the matrix.
    await w.setProps({ bump: 1 })
    await flushPromises()
    expect(cells(w)[0].classes()).toContain('cell-deny')
    expect(cells(w)[0].classes()).not.toContain('cell-exception')
    await cells(w)[0].trigger('click')
    expect(w.find('.exception-block').exists()).toBe(false)
    expect(w.find('.exception-form').exists()).toBe(true) // can be granted anew
  })
})
