import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'
import AppleIDView from '../AppleIDView.vue'
import en from '@/i18n/locales/en/appleid'
import zh from '@/i18n/locales/zh/appleid'
import type { AppleIDChallenge } from '@/api/admin/appleid'

const { initializeAppleID } = vi.hoisted(() => ({ initializeAppleID: vi.fn() }))
vi.mock('@/api/admin/appleid', () => ({ initializeAppleID }))

enableAutoUnmount(afterEach)
beforeEach(() => vi.resetAllMocks())

const challenge: AppleIDChallenge = { image_base64: '/9j/4A==', mime_type: 'image/jpeg', image_bytes: 4 }
const renderView = (locale = 'zh') => mount(AppleIDView, {
  global: {
    plugins: [createI18n({ legacy: false, locale, messages: { zh: { appleid: zh }, en: { appleid: en } } })],
    stubs: { AppLayout: { template: '<div><slot /></div>' } },
  },
})

describe('independent Apple ID workbench', () => {
  it('starts idle without calling upstream or showing registration success', () => {
    const wrapper = renderView()
    expect(initializeAppleID).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('准备开始')
    expect(wrapper.get('[data-step="03"]').text()).toContain('尚未接入')
    expect(wrapper.get('[data-step="04"]').text()).toContain('尚未接入')
    expect(wrapper.find('input').exists()).toBe(false)
    expect(wrapper.get('a[target="_blank"]').attributes('rel')).toBe('noopener noreferrer')
  })

  it('disables duplicate requests and renders the actual returned image', async () => {
    let complete: ((value: AppleIDChallenge) => void) | undefined
    initializeAppleID.mockImplementation(() => new Promise<AppleIDChallenge>(resolve => { complete = resolve }))
    const wrapper = renderView()
    await wrapper.get('[data-testid="initialize"]').trigger('click')
    expect(wrapper.get('[data-testid="initialize"]').attributes('disabled')).toBeDefined()
    expect(wrapper.text()).toContain('正在连接 Apple')
    await wrapper.get('[data-testid="initialize"]').trigger('click')
    expect(initializeAppleID).toHaveBeenCalledTimes(1)
    complete?.(challenge)
    await flushPromises()
    expect(wrapper.get('img').attributes('src')).toBe('data:image/jpeg;base64,/9j/4A==')
    expect(wrapper.get('[data-step="02"]').text()).toContain('已完成')
    expect(wrapper.get('[data-step="04"]').text()).toContain('尚未接入')
    await wrapper.get('[data-testid="reset"]').trigger('click')
    expect(wrapper.find('img').exists()).toBe(false)
    expect(wrapper.text()).toContain('准备开始')
  })

  it('shows a retryable error without rendering upstream internals', async () => {
    initializeAppleID.mockRejectedValue(new Error('private-session-token'))
    const wrapper = renderView()
    await wrapper.get('[data-testid="initialize"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('本次初始化未完成')
    expect(wrapper.text()).not.toContain('private-session-token')
    initializeAppleID.mockResolvedValue(challenge)
    await wrapper.get('[data-testid="initialize"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('img').exists()).toBe(true)
  })

  it('aborts on cancel and ignores late results', async () => {
    let complete: ((value: AppleIDChallenge) => void) | undefined
    initializeAppleID.mockImplementation(() => new Promise<AppleIDChallenge>(resolve => { complete = resolve }))
    const wrapper = renderView()
    await wrapper.get('[data-testid="initialize"]').trigger('click')
    const signal: AbortSignal = initializeAppleID.mock.calls[0][0]
    await wrapper.get('[data-testid="reset"]').trigger('click')
    expect(signal.aborted).toBe(true)
    complete?.(challenge)
    await flushPromises()
    expect(wrapper.find('img').exists()).toBe(false)
    expect(wrapper.text()).toContain('准备开始')
  })

  it('aborts an outstanding request on navigation', async () => {
    initializeAppleID.mockImplementation(() => new Promise(() => {}))
    const wrapper = renderView()
    await wrapper.get('[data-testid="initialize"]').trigger('click')
    const signal: AbortSignal = initializeAppleID.mock.calls[0][0]
    wrapper.unmount()
    expect(signal.aborted).toBe(true)
  })

  it('supports the existing English locale', () => {
    expect(renderView('en').text()).toContain('Apple ID Workbench')
  })
})
