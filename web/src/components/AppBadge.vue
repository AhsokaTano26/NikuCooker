<script setup lang="ts">
/**
 * A small label beside something else.
 *
 * Used for the provenance chips on the settings page and the status words in
 * tables. A `<span>` with classes rather than a `<div>` with a border: a badge
 * sits inside a line of text and must not break it onto its own.
 */
withDefaults(
  defineProps<{
    tone?: 'neutral' | 'accent' | 'done' | 'warn' | 'failed'
  }>(),
  { tone: 'neutral' },
)

const TONES: Record<string, string> = {
  neutral: 'bg-surface-sunken text-ink-faint',
  accent: 'bg-accent/15 text-accent',
  done: 'text-status-done',
  warn: 'text-status-warn',
  failed: 'text-status-failed',
}
</script>

<template>
  <span class="rounded px-1.5 py-0.5 text-xs" :class="TONES[tone]">
    <slot />
  </span>
</template>
