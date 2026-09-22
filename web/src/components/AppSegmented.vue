<script setup lang="ts">
/**
 * A row of mutually exclusive choices, all visible at once.
 *
 * Used where a dropdown would be worse: three or four short options that are
 * read at a glance and switched often, so putting them behind a click would
 * cost more than it saved. The whole point is that the alternatives stay on
 * screen.
 *
 * `role="radiogroup"` rather than a set of buttons, because that is what this
 * is — choosing one clears the others — and it is what tells a screen reader
 * to announce the options together and which one is current.
 */
withDefaults(
  defineProps<{
    options: { value: string; label: string }[]
    disabled?: boolean
  }>(),
  { disabled: false },
)

const model = defineModel<string>({ default: '' })
</script>

<template>
  <div
    role="radiogroup"
    class="inline-flex overflow-hidden rounded border border-line"
    :class="disabled ? 'opacity-50' : ''"
  >
    <button
      v-for="option in options"
      :key="option.value"
      type="button"
      role="radio"
      :aria-checked="model === option.value"
      :disabled="disabled"
      class="px-3 py-1.5 text-sm transition outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent disabled:cursor-not-allowed"
      :class="
        model === option.value
          ? 'bg-accent text-accent-ink'
          : 'bg-surface-raised text-ink-muted hover:text-ink'
      "
      @click="model = option.value"
    >
      {{ option.label }}
    </button>
  </div>
</template>
