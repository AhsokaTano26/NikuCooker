<script setup lang="ts">
/**
 * A button.
 *
 * The variants are the ones this interface actually distinguishes: the action a
 * page exists for, the actions beside it, and the ones that undo something. A
 * single `class` prop would let every caller invent its own spacing and weight,
 * which is how a set of buttons stops looking like a set.
 */
withDefaults(
  defineProps<{
    variant?: 'primary' | 'secondary' | 'ghost' | 'danger'
    size?: 'sm' | 'md'
    disabled?: boolean
    type?: 'button' | 'submit'
  }>(),
  { variant: 'secondary', size: 'md', disabled: false, type: 'button' },
)

const VARIANTS: Record<string, string> = {
  primary:
    'bg-accent text-accent-ink font-medium hover:opacity-90 focus-visible:outline-accent',
  secondary:
    'border border-line text-ink-muted hover:border-ink-faint hover:text-ink focus-visible:outline-accent',
  ghost: 'text-ink-muted hover:bg-surface-raised hover:text-ink focus-visible:outline-accent',
  danger:
    'border border-status-failed/60 text-status-failed hover:bg-status-failed/10 focus-visible:outline-status-failed',
}

const SIZES: Record<string, string> = {
  sm: 'px-2 py-1 text-xs',
  md: 'px-3 py-1.5 text-sm',
}
</script>

<template>
  <button
    :type="type"
    :disabled="disabled"
    class="rounded transition outline-none focus-visible:outline-2 focus-visible:outline-offset-2 disabled:cursor-not-allowed disabled:opacity-40"
    :class="[VARIANTS[variant], SIZES[size]]"
  >
    <slot />
  </button>
</template>
