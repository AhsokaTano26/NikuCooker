<script setup lang="ts">
import { useRoute } from 'vue-router'

import PagePlaceholder from '@/components/PagePlaceholder.vue'
import type { NavMeta } from '@/router'

// Title, phase and description come from the route definition so there is one
// source of truth for them. This file is replaced by the real view in the phase
// its route meta names.
const meta = useRoute().meta as unknown as NavMeta
</script>

<template>
  <PagePlaceholder :title="meta.label" :phase="meta.phase" :description="meta.description" />
</template>
