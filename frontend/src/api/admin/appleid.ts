import { apiClient } from '../client'

export interface AppleIDChallenge {
  readonly image_base64: string
  readonly mime_type: 'image/jpeg' | 'image/png'
  readonly image_bytes: number
}

export async function initializeAppleID(signal: AbortSignal): Promise<AppleIDChallenge> {
  const { data } = await apiClient.post<AppleIDChallenge>('/admin/appleid/bootstrap', {}, { signal })
  return data
}
