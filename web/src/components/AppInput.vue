<script setup lang="ts">
/**
 * A text-like input.
 *
 * The native element stays: it is what a keyboard, a password manager and a
 * screen reader already know how to talk to, and replacing it with a div would
 * mean rebuilding three things that work. What changes is the appearance —
 * `appearance-none` removes the platform's own drawing, and the classes below
 * draw it instead.
 */
withDefaults(
  defineProps<{
    type?: 'text' | 'search' | 'number' | 'password' | 'email'
    /** Rendered in the monospace face. For paths, identifiers and keys, where
     *  a proportional font makes characters that matter hard to tell apart. */
    mono?: boolean
    disabled?: boolean
    placeholder?: string
    min?: number | string
    max?: number | string
    step?: number | string
    id?: string
  }>(),
  {
    type: 'text',
    mono: false,
    disabled: false,
    placeholder: '',
    min: undefined,
    max: undefined,
    step: undefined,
    id: undefined,
  },
)

const model = defineModel<string>({ default: '' })
</script>

<template>
  <input
    :id="id"
    v-model="model"
    :type="type"
    :disabled="disabled"
    :placeholder="placeholder"
    :min="min"
    :max="max"
    :step="step"
    class="w-full appearance-none rounded border border-line bg-surface px-2 py-1.5 text-sm text-ink outline-none transition placeholder:text-ink-faint hover:border-ink-faint focus:border-accent focus:ring-1 focus:ring-accent disabled:cursor-not-allowed disabled:opacity-50"
    :class="mono ? 'font-mono text-xs' : ''"
  />
</template>
