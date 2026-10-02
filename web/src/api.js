// Thin JSON client for the simulator API.
async function request(path, options = {}) {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...options,
  })
  const body = await res.json().catch(() => ({}))
  if (!res.ok) {
    const err = new Error(body.error || `${res.status} ${res.statusText}`)
    err.status = res.status
    err.code = body.code
    throw err
  }
  return body
}

export const api = {
  state: () => request('/api/state'),
  saveDraft: (doc) => request('/api/draft', { method: 'PUT', body: JSON.stringify(doc) }),
  preview: () => request('/api/preview', { method: 'POST' }),
  publish: (payload) => request('/api/publish', { method: 'POST', body: JSON.stringify(payload) }),
  decisions: (version) => request(`/api/decisions/${version}`),
  reset: () => request('/api/demo/reset', { method: 'POST' }),
}
