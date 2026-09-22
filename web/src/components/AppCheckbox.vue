<script setup lang="ts">
/**
 * A checkbox.
 *
 * The native input is still there, laid over the drawn box at zero opacity.
 * That is deliberate rather than lazy: it keeps the space bar, the focus ring,
 * the screen reader's "checked" announcement and the label association working,
 * none of which a div reproduces without being taught. What the user sees is
 * ours; what does the work is still the browser's.
 *
 * `appearance-none` is what stops the platform from drawing its own box on top.
 */
withDefaults(
  defineProps<{
    disabled?: boolean
    /** Shown beside the box. Omitted where a surrounding label already says it. */
    label?: string
  }>(),
  { disabled: false, label: '' },
)

const model = defineModel<boolean>({ default: false })
</script>

<template>
  <label
    class="inline-flex items-start gap-2"
    :class="disabled ? 'cursor-not-allowed opacity-50' : 'cursor-pointer'"
  >
    <span class="relative mt-0.5 flex size-4 shrink-0 items-center justify-center">
      <input
        v-model="model"
        type="checkbox"
        :disabled="disabled"
        class="peer absolute inset-0 size-4 cursor-pointer appearance-none rounded border border-line bg-surface transition checked:border-accent checked:bg-accent hover:border-ink-faint focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent disabled:cursor-not-allowed"
      />
      <!-- Drawn rather than typed: a glyph would sit on the text baseline and
           move with the font, and a tick has to be centred in a 16px box. -->
      <svg
        viewBox="0 0 12 12"
        class="pointer-events-none relative size-3 text-accent-ink opacity-0 transition peer-checked:opacity-100"
        aria-hidden="true"
      >
        <path
          d="M2.5 6.2 L4.8 8.6 L9.5 3.6"
          fill="none"
          stroke="currentColor"
          stroke-width="1.8"
          stroke-linecap="round"
          stroke-linejoin="round"
        />
      </svg>
    </span>
    <slot>
      <span v-if="label" class="text-sm text-ink-muted">{{ label }}</span>
    </slot>
  </label>
</template>
