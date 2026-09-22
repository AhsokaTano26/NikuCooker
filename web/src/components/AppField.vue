<script setup lang="ts">
/**
 * A label, a control, and whatever must be said about it.
 *
 * Every form in this interface is the same three things in the same order, and
 * writing them out per field is how one ends up without a label — which is the
 * one that somebody cannot fill in.
 */
withDefaults(
  defineProps<{
    label?: string
    /** One line under the control. What it does, not what it is called. */
    help?: string
    /** The configuration key or field name, in monospace. Optional. */
    path?: string
    /** The id of the control this labels. Named for what it holds rather than
     *  `for`, which is a reserved word and cannot be a prop. */
    forId?: string
  }>(),
  { label: '', help: '', path: '', forId: undefined },
)
</script>

<template>
  <div class="min-w-0">
    <div v-if="label || $slots.aside" class="flex flex-wrap items-center gap-2">
      <label v-if="label" :for="forId" class="text-sm text-ink">{{ label }}</label>
      <slot name="aside" />
    </div>

    <div class="mt-1">
      <slot />
    </div>

    <p v-if="help" class="mt-1 text-xs text-ink-faint">{{ help }}</p>
    <p v-if="path" class="mt-0.5 font-mono text-xs text-ink-faint">{{ path }}</p>
  </div>
</template>
