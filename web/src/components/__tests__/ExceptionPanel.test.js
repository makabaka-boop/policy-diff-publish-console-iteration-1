import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

const apiMocks = vi.hoisted(() => ({
  listExceptions: vi.fn(),
  createException: vi.fn(),
}))
vi.mock('../../api.js', () => ({
  api: {
    listExceptions: apiMocks.listExceptions,
    createException: apiMocks.createException,
  },
}))

import ExceptionPanel from '../ExceptionPanel.vue'

const publishedDoc = {
  roles: [{ name: 'base' }, { name: 'viewer' }],
  resources: ['doc', 'billing'],
  actions: ['read', 'write'],
}

const activeException = {
  id: 'exc-p1-1',
  tuple: { role: 'viewer', resource: 'billing', action: 'read' },
  reason: 'drill step 3',
  createdAt: '2026-10-02T09:00:00Z',
  expiresAt: '2026-10-02T09:10:00Z',
  publishedRevision: 1,
}

function mountPanel(props = {}) {
  return mount(ExceptionPanel, {
    props: {
      publishedDoc,
      publishedRevision: 1,
      bump: 0,
      ...props,
    },
  })
}

beforeEach(() => {
  vi.clearAllMocks()
})

describe('ExceptionPanel', () => {
  it('lists active exceptions pinned to the current revision and counts down', async () => {
    apiMocks.listExceptions.mockResolvedValue({
      publishedRevision: 1,
      exceptions: [activeException],
    })
    const wrapper = mountPanel()
    await flushPromises()

    const text = wrapper.text()
    expect(text).toContain('viewer / billing / read')
    expect(text).toContain('exc-p1-1')
    expect(text).toContain('p1')
    expect(text).toContain('drill step 3')
    expect(text).toContain('生效中的例外（1）')
  })

  it('hides exceptions pinned to a stale revision after a publish bump', async () => {
    apiMocks.listExceptions.mockResolvedValue({
      publishedRevision: 2,
      exceptions: [{ ...activeException, publishedRevision: 1 }],
    })
    const wrapper = mountPanel({ publishedRevision: 2, bump: 3 })
    await flushPromises()
    expect(wrapper.text()).toContain('生效中的例外（0）')
    expect(wrapper.text()).not.toContain('exc-p1-1')
  })

  it('rejects an empty reason without calling the API', async () => {
    apiMocks.listExceptions.mockResolvedValue({ publishedRevision: 1, exceptions: [] })
    const wrapper = mountPanel()
    await flushPromises()

    // Defaults select concrete tuple values; leave reason empty.
    await wrapper.find('button.btn-primary').trigger('click')
    await flushPromises()

    expect(apiMocks.createException).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('必须填写明确的放行理由')
  })

  it('submits the request and emits created + refreshes on success', async () => {
    apiMocks.listExceptions
      .mockResolvedValueOnce({ publishedRevision: 1, exceptions: [] })
      .mockResolvedValueOnce({ publishedRevision: 1, exceptions: [activeException] })
    apiMocks.createException.mockResolvedValue({})
    const wrapper = mountPanel()
    await flushPromises()

    await wrapper.find('input.reason-input').setValue('instructor drill override')
    await wrapper.find('input.ttl-input').setValue(7)
    await wrapper.find('button.btn-primary').trigger('click')
    await flushPromises()

    expect(apiMocks.createException).toHaveBeenCalledTimes(1)
    const payload = apiMocks.createException.mock.calls[0][0]
    expect(payload).toMatchObject({
      role: 'base',
      resource: 'doc',
      action: 'read',
      ttlMinutes: 7,
      reason: 'instructor drill override',
    })
    expect(wrapper.emitted('created')).toBeTruthy()
    expect(wrapper.text()).toContain('生效中的例外（1）')
  })

  it('surfaces the server rejection for an already-allowed tuple as a whole-request error', async () => {
    apiMocks.listExceptions.mockResolvedValue({ publishedRevision: 1, exceptions: [] })
    apiMocks.createException.mockRejectedValue(
      Object.assign(new Error('tuple is not denied by the currently published policy'), {
        status: 422,
        code: 'tuple_not_denied',
      }),
    )
    const wrapper = mountPanel()
    await flushPromises()

    await wrapper.find('input.reason-input').setValue('bad idea')
    await wrapper.find('button.btn-primary').trigger('click')
    await flushPromises()

    expect(wrapper.find('.banner-err').text()).toContain('tuple_not_denied')
    expect(wrapper.emitted('created')).toBeFalsy()
  })
})
