<script setup lang="ts">
/**
 * One choice in a radio group.
 *
 * Rendered as a card rather than a dot with a word beside it, because every
 * group in this interface is a choice between described alternatives — a
 * translation style, an upload source — and the description is the part that
 * decides. The dot is kept so the control still reads as a single choice
 * rather than a list of buttons.
 *
 * Like the checkbox, the native input is present and invisible: it carries the
 * radio *group* semantics — arrow keys move between options and only one can
 * be selected — which is the part a hand-rolled equivalent gets wrong.
 */
defineProps<{
  value: string
  label: string
  hint?: string
  disabled?: boolean
}>()

const model = defineModel<string>({ required: true })
</script>

<template>
  <label
    class="flex cursor-pointer gap-3 rounded border p-3 transition"
    :class="
      model === value
        ? 'border-accent bg-surface-raised'
        : 'border-line hover:border-ink-faint'
    "
  >
    <span class="relative mt-0.5 flex size-4 shrink-0 items-center justify-center">
      <input
        v-model="model"
        type="radio"
        :value="value"
        :disabled="disabled"
        class="peer absolute inset-0 size-4 cursor-pointer appearance-none rounded-full border border-line bg-surface transition checked:border-accent hover:border-ink-faint focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
      />
      <span
        class="pointer-events-none relative size-1.5 rounded-full bg-accent opacity-0 transition peer-checked:opacity-100"
      />
    </span>

    <span class="min-w-0">
      <span class="block text-sm">{{ label }}</span>
      <span v-if="hint" class="mt-0.5 block text-xs text-ink-faint">{{ hint }}</span>
    </span>
  </label>
</template>
