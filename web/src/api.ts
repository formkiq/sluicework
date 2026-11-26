export const base = '/api/v1'
export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message)
  }
}
export async function api<T>(path: string, method = 'GET', body?: unknown): Promise<T> {
  const response = await fetch(base + path, {
    method,
    credentials: 'same-origin',
    headers: {
      Accept: 'application/json',
      ...(method !== 'GET' ? { 'Content-Type': 'application/json' } : {}),
    },
    body: method === 'GET' ? undefined : JSON.stringify(body ?? {}),
  })
  if (!response.ok) {
    let message = response.statusText || 'Request failed'
    try {
      const data = await response.json()
      message = data.error || data.message || message
    } catch {
      /* Non-JSON proxy response. */
    }
    if (response.status === 401 && path !== '/login')
      window.dispatchEvent(new Event('session-expired'))
    throw new ApiError(response.status, message)
  }
  if (response.status === 204) return undefined as T
  const raw = await response.text()
  return raw ? JSON.parse(raw) : (undefined as T)
}
export const idPath = (path: string, id: string) => path + '/' + encodeURIComponent(id)
