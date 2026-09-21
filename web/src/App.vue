<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted } from 'vue'
import { RouterLink, RouterView, useRoute } from 'vue-router'

import { navItems } from '@/router'
import { useEventStore } from '@/stores/events'
import type { ConnectionState } from '@/stores/events'

const events = useEventStore()
const route = useRoute()

const currentTitle = computed(() => {
  const label = route.meta['label']
  return typeof label === 'string' ? label : 'NikuCooker'
})

/**
 * Connection state is displayed rather than hidden.
 *
 * "The event stream is down" and "the pipeline is paused" look identical in a
 * UI that does not distinguish them, and the difference decides whether a user
 * waits or reports a bug.
 */
const connectionLabel: Record<ConnectionState, string> = {
  idle: '未连接',
  connecting: '连接中',
  live: '已连接',
  reconnecting: '重连中',
  resyncing: '重新同步',
}

const connectionTone: Record<ConnectionState, string> = {
  idle: 'bg-status-pending',
  connecting: 'bg-status-running',
  live: 'bg-status-done',
  reconnecting: 'bg-status-failed',
  resyncing: 'bg-status-running',
}

onMounted(() => {
  // A single stream for the whole application, not one per view.
  events.connect()
})

onBeforeUnmount(() => {
  events.disconnect()
})
</script>

<template>
  <div class="flex h-full">
    <aside class="flex w-60 shrink-0 flex-col border-r border-line bg-surface-sunken">
      <div class="px-5 py-5">
        <p class="text-base font-semibold tracking-tight">NikuCooker</p>
        <p class="mt-0.5 text-xs text-ink-faint">Local-first 烤肉机</p>
      </div>

      <nav class="flex-1 space-y-0.5 px-2">
        <RouterLink
          v-for="item in navItems"
          :key="item.name"
          :to="item.path"
          class="block rounded-md px-3 py-2 text-sm text-ink-muted transition-colors hover:bg-surface-raised hover:text-ink"
          active-class="bg-surface-raised text-ink font-medium"
        >
          {{ item.label }}
        </RouterLink>
      </nav>

      <div class="border-t border-line px-5 py-3">
        <div class="flex items-center gap-2">
          <span
            class="size-2 shrink-0 rounded-full"
            :class="connectionTone[events.connection]"
            aria-hidden="true"
          />
          <span class="text-xs text-ink-faint">{{ connectionLabel[events.connection] }}</span>
        </div>
        <p v-if="events.lastError" class="mt-1.5 text-xs leading-snug text-status-failed">
          {{ events.lastError }}
        </p>
      </div>
    </aside>

    <div class="flex min-w-0 flex-1 flex-col">
      <header
        class="flex h-14 shrink-0 items-center justify-between border-b border-line px-8"
        aria-live="polite"
      >
        <h1 class="text-sm font-medium text-ink-muted">{{ currentTitle }}</h1>
      </header>

      <main class="min-h-0 flex-1 overflow-auto">
        <RouterView />
      </main>
    </div>
  </div>
</template>
