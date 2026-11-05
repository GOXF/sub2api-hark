import { describe, expect, it, vi } from 'vitest'

vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ checkAuth: vi.fn(), isAuthenticated: false, isAdmin: false, isSimpleMode: false }) }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ siteName: 'Sub2API', backendModeEnabled: false, cachedPublicSettings: null }) }))
vi.mock('@/stores/adminSettings', () => ({ useAdminSettingsStore: () => ({ customMenuItems: [] }) }))
vi.mock('@/stores/adminCompliance', () => ({ useAdminComplianceStore: () => ({}) }))
vi.mock('@/composables/useNavigationLoading', () => ({ useNavigationLoadingState: () => ({ startNavigation: vi.fn(), endNavigation: vi.fn() }) }))
vi.mock('@/composables/useRoutePrefetch', () => ({ useRoutePrefetch: () => ({ triggerPrefetch: vi.fn() }) }))
vi.mock('@/views/auth/LoginView.vue', () => ({ default: { template: '<div>Login</div>' } }))

describe('standalone Apple ID route', () => {
  it('uses an admin-only lazy-loaded page and sends anonymous users to login', async () => {
    vi.spyOn(window, 'scrollTo').mockImplementation(() => {})
    const { initI18n } = await import('@/i18n')
    await initI18n()
    const { default: router } = await import('@/router')
    const route = router.getRoutes().find(record => record.name === 'AdminAppleID')
    expect(route?.path).toBe('/admin/appleid')
    expect(route?.meta.requiresAuth).toBe(true)
    expect(route?.meta.requiresAdmin).toBe(true)
    expect(route?.meta.titleKey).toBe('appleid.title')
    await router.push('/admin/appleid')
    expect(router.currentRoute.value.path).toBe('/login')
    expect(router.currentRoute.value.query.redirect).toBe('/admin/appleid')
  })
})
