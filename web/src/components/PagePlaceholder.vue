<script setup lang="ts">
/**
 * Placeholder for a view whose phase has not been implemented yet.
 *
 * It names the phase rather than showing an empty page: a blank screen is
 * indistinguishable from a bug, and claiming a feature exists when it does not
 * is worse than either. Every view using this is replaced by its phase.
 */
defineProps<{
  title: string
  phase: string
  description: string
}>()
</script>

<template>
  <section class="mx-auto max-w-3xl px-8 py-12">
    <header class="flex items-baseline gap-3">
      <h1 class="text-2xl font-semibold tracking-tight">{{ title }}</h1>
      <span
        class="rounded-full border border-line px-2.5 py-0.5 text-xs font-medium text-ink-faint"
      >
        {{ phase }}
      </span>
    </header>

    <p class="mt-3 text-sm leading-relaxed text-ink-muted">{{ description }}</p>

    <p class="mt-8 rounded-lg border border-line bg-surface-raised px-4 py-3 text-sm text-ink-muted">
      此视图尚未实现。当前处于 <strong class="font-medium text-ink">Phase 0</strong>：仓库骨架、协议与
      构建工具链。参见
      <code class="rounded bg-surface-sunken px-1.5 py-0.5 text-xs">docs/roadmap.md</code> 了解各阶段交付内容。
    </p>
  </section>
</template>
