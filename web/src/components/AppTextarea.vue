<script setup lang="ts">
/**
 * A multi-line text input.
 *
 * Same reasoning as the single-line one: the native element carries the
 * behaviour, `appearance-none` removes the platform's drawing and the classes
 * draw it instead. `resize-y` rather than `resize`: a line of subtitles is
 * short by nature, and letting it be dragged wider than its column breaks the
 * row it sits in.
 */
withDefaults(defineProps<{ rows?: number; disabled?: boolean; placeholder?: string }>(), {
  rows: 3,
  disabled: false,
  placeholder: '',
})
</script>

<template>
  <textarea
    :rows="rows"
    :disabled="disabled"
    :placeholder="placeholder"
    class="w-full resize-y appearance-none rounded border border-line bg-surface px-2 py-1.5 text-sm text-ink outline-none transition placeholder:text-ink-faint hover:border-ink-faint focus:border-accent focus:ring-1 focus:ring-accent disabled:cursor-not-allowed disabled:opacity-50"
  ></textarea>
</template>
