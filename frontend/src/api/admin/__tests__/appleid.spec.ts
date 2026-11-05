import { beforeEach, describe, expect, it, vi } from 'vitest'
import { initializeAppleID } from '../appleid'

const { post } = vi.hoisted(() => ({ post: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { post } }))
beforeEach(() => vi.resetAllMocks())

describe('Apple ID admin API', () => {
  it('uses the separate endpoint and passes request cancellation', async () => {
    const data = { image_base64: 'fixture', mime_type: 'image/jpeg', image_bytes: 7 }
    post.mockResolvedValue({ data })
    const signal = new AbortController().signal
    expect(await initializeAppleID(signal)).toEqual(data)
    expect(post).toHaveBeenCalledWith('/admin/appleid/bootstrap', {}, { signal })
  })
})
