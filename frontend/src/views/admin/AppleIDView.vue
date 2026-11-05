<script setup lang="ts">
import { computed, onUnmounted, shallowRef } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import Icon from '@/components/icons/Icon.vue'
import { initializeAppleID, type AppleIDChallenge } from '@/api/admin/appleid'

type WorkbenchState =
  | { readonly status: 'idle' | 'loading' | 'error' }
  | { readonly status: 'ready'; readonly challenge: AppleIDChallenge }

const { t } = useI18n()
const state = shallowRef<WorkbenchState>({ status: 'idle' })
let requestController: AbortController | null = null

const loading = computed(() => state.value.status === 'loading')
const challenge = computed(() => state.value.status === 'ready' ? state.value.challenge : null)
const imageURL = computed(() => challenge.value
  ? `data:${challenge.value.mime_type};base64,${challenge.value.image_base64}`
  : '')
const buttonText = computed(() => {
  if (state.value.status === 'ready') return t('appleid.refresh')
  if (state.value.status === 'error') return t('appleid.retry')
  return t('appleid.start')
})
const connectedStatus = computed(() => {
  if (loading.value) return t('appleid.working')
  return challenge.value ? t('appleid.ready') : t('appleid.waiting')
})
const steps = computed(() => [
  { number: '01', title: t('appleid.initialize'), hint: t('appleid.initializeHint'), status: connectedStatus.value, connected: true },
  { number: '02', title: t('appleid.captcha'), hint: t('appleid.captchaHint'), status: connectedStatus.value, connected: true },
  { number: '03', title: t('appleid.email'), hint: t('appleid.emailHint'), status: t('appleid.notConnected'), connected: false },
  { number: '04', title: t('appleid.create'), hint: t('appleid.createHint'), status: t('appleid.notConnected'), connected: false },
])

async function initialize(): Promise<void> {
  if (loading.value) return
  const controller = new AbortController()
  requestController = controller
  state.value = { status: 'loading' }
  try {
    const result = await initializeAppleID(controller.signal)
    if (!controller.signal.aborted) state.value = { status: 'ready', challenge: result }
  } catch {
    if (!controller.signal.aborted) state.value = { status: 'error' }
  } finally {
    if (requestController === controller) requestController = null
  }
}

function reset(): void {
  requestController?.abort()
  requestController = null
  state.value = { status: 'idle' }
}

onUnmounted(reset)
</script>

<template>
  <AppLayout>
    <div class="mx-auto max-w-6xl space-y-6">
      <header class="border-b border-gray-200 pb-5 dark:border-dark-700">
        <div class="flex flex-wrap items-center gap-3">
          <h1 class="text-2xl font-semibold tracking-tight text-gray-900 dark:text-white">{{ t('appleid.title') }}</h1>
          <span class="rounded-md border border-primary-200 bg-primary-50 px-2 py-1 text-xs font-medium text-primary-700 dark:border-primary-800 dark:bg-primary-950/40 dark:text-primary-300">{{ t('appleid.independent') }}</span>
        </div>
        <p class="mt-2 text-sm text-gray-500 dark:text-gray-400">{{ t('appleid.description') }}</p>
      </header>

      <div class="rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-sm leading-6 text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/30 dark:text-amber-200">{{ t('appleid.scope') }}</div>

      <div class="grid items-start gap-6 lg:grid-cols-[320px_minmax(0,1fr)]">
        <section class="rounded-xl border border-gray-200 bg-white p-5 dark:border-dark-700 dark:bg-dark-900" :aria-label="t('appleid.progress')">
          <h2 class="mb-5 text-sm font-semibold text-gray-900 dark:text-white">{{ t('appleid.progress') }}</h2>
          <ol class="space-y-6">
            <li v-for="step in steps" :key="step.number" class="flex gap-3" :data-step="step.number">
              <span class="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg border font-mono text-xs" :class="step.connected && challenge ? 'border-primary-200 bg-primary-50 text-primary-700 dark:border-primary-800 dark:bg-primary-950/40 dark:text-primary-300' : 'border-gray-200 text-gray-400 dark:border-dark-600'">{{ step.number }}</span>
              <div class="min-w-0 flex-1">
                <p class="text-sm font-medium text-gray-900 dark:text-gray-100">{{ step.title }}</p>
                <p class="mt-1 text-xs leading-5 text-gray-500 dark:text-gray-400">{{ step.hint }}</p>
                <p class="mt-1 text-xs" :class="step.connected && challenge ? 'text-primary-600 dark:text-primary-400' : 'text-gray-400'">{{ step.status }}</p>
              </div>
            </li>
          </ol>
        </section>

        <section class="overflow-hidden rounded-xl border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-900" :aria-busy="loading" :aria-label="t('appleid.panelTitle')">
          <div class="border-b border-gray-100 p-5 dark:border-dark-700">
            <h2 class="font-semibold text-gray-900 dark:text-white">{{ t('appleid.panelTitle') }}</h2>
            <p class="mt-1 text-sm leading-6 text-gray-500 dark:text-gray-400">{{ t('appleid.panelDescription') }}</p>
          </div>

          <div class="flex min-h-80 flex-col items-center justify-center px-6 py-10 text-center" role="status" aria-live="polite">
            <template v-if="state.status === 'idle'">
              <Icon name="shield" size="xl" class="mb-4 text-gray-400" />
              <h3 class="font-medium text-gray-900 dark:text-white">{{ t('appleid.idleTitle') }}</h3>
              <p class="mt-2 max-w-sm text-sm leading-6 text-gray-500 dark:text-gray-400">{{ t('appleid.idleDescription') }}</p>
            </template>
            <template v-else-if="loading">
              <Icon name="refresh" size="xl" class="mb-4 animate-spin text-primary-500 motion-reduce:animate-none" />
              <h3 class="font-medium text-gray-900 dark:text-white">{{ t('appleid.loadingTitle') }}</h3>
              <p class="mt-2 max-w-sm text-sm leading-6 text-gray-500 dark:text-gray-400">{{ t('appleid.loadingDescription') }}</p>
            </template>
            <template v-else-if="challenge">
              <h3 class="font-medium text-gray-900 dark:text-white">{{ t('appleid.readyTitle') }}</h3>
              <div class="mt-5 rounded-lg border border-gray-200 bg-white p-4">
                <img :src="imageURL" :alt="t('appleid.imageAlt')" class="h-auto max-w-full" />
              </div>
              <p class="mt-2 font-mono text-xs text-gray-400">{{ t('appleid.imageSize', { count: challenge.image_bytes }) }}</p>
              <p class="mt-4 max-w-sm text-sm leading-6 text-gray-500 dark:text-gray-400">{{ t('appleid.readyDescription') }}</p>
            </template>
            <template v-else>
              <Icon name="exclamationCircle" size="xl" class="mb-4 text-amber-500" />
              <h3 class="font-medium text-gray-900 dark:text-white">{{ t('appleid.errorTitle') }}</h3>
              <p class="mt-2 max-w-sm text-sm leading-6 text-gray-500 dark:text-gray-400">{{ t('appleid.errorDescription') }}</p>
            </template>
          </div>

          <div class="flex flex-wrap gap-3 border-t border-gray-100 p-5 dark:border-dark-700">
            <button type="button" class="btn btn-primary" :disabled="loading" data-testid="initialize" @click="initialize">
              <Icon name="refresh" size="sm" />
              {{ loading ? t('appleid.working') : buttonText }}
            </button>
            <button v-if="state.status !== 'idle'" type="button" class="btn btn-secondary" data-testid="reset" @click="reset">{{ loading ? t('appleid.cancel') : t('appleid.reset') }}</button>
          </div>
        </section>
      </div>

      <footer class="flex flex-col gap-3 text-xs text-gray-500 dark:text-gray-400 sm:flex-row sm:items-start sm:justify-between">
        <p>{{ t('appleid.isolation') }}</p>
        <div class="sm:text-right">
          <a href="https://account.apple.com/account#!&page=create" target="_blank" rel="noopener noreferrer" class="font-medium text-primary-600 hover:underline dark:text-primary-400">{{ t('appleid.official') }} ↗</a>
          <p class="mt-1">{{ t('appleid.officialHint') }}</p>
        </div>
      </footer>
    </div>
  </AppLayout>
</template>
