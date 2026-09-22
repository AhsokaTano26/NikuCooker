<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'

import { api, type ListProjectsQuery } from '@/api/client'
import AppSegmented from '@/components/AppSegmented.vue'
import AppButton from '@/components/AppButton.vue'
import AppInput from '@/components/AppInput.vue'
import { formatDuration, useAsync } from '@/composables/useAsync'
import { useEventStore } from '@/stores/events'
import type { Project } from '@/types/api'

const events = useEventStore()

/** The three views of the project list, in the order they are read. */
const STATUS_FILTERS = [
  { value: 'active', label: '进行中' },
  { value: 'archived', label: '已归档' },
  { value: 'all', label: '全部' },
]

const filter = ref<'active' | 'archived' | 'all'>('active')
const search = ref('')

const projects = useAsync(() => api.projects.list(query()))

function query(): ListProjectsQuery {
  const value: ListProjectsQuery = { status: filter.value, limit: 100 }
  if (search.value.trim() !== '') value.q = search.value.trim()
  return value
}

onMounted(projects.run)

// Refetched when a project changes anywhere — including from another tab, or
// from a run finishing. The list is the thing a user leaves open.
watch(() => events.resyncCount, projects.run)

let searchTimer: ReturnType<typeof setTimeout> | null = null
watch(search, () => {
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(projects.run, 250)
})

function stageLabel(project: Project): string {
  if (project.current_job) {
    return `${project.current_job.status} · ${(project.current_job.progress * 100).toFixed(0)}%`
  }
  if (project.segment_count === 0) return '尚未运行'
  return `${project.segment_count} 行`
}

const actionError = ref<string | null>(null)

async function remove(project: Project): Promise<void> {
  actionError.value = null
  // Two deliberate steps: the browser's own modal is a real interruption, and
  // the API refuses an unconfirmed delete anyway. Deleting a project's files is
  // unrecoverable and the confirmation is the only thing standing in front of it.
  if (!confirm(`删除项目「${project.name}」？文件会保留。`)) return

  try {
    await api.projects.remove(project.id, { confirm: true, deleteFiles: false })
    await projects.run()
  } catch (cause) {
    actionError.value = cause instanceof Error ? cause.message : String(cause)
  }
}
</script>

<template>
  <div class="space-y-4">
    <div class="flex flex-wrap items-center gap-3">
      <AppSegmented v-model="filter" :options="STATUS_FILTERS" />

      <div class="min-w-56 flex-1">
        <AppInput v-model="search" type="search" placeholder="搜索项目名" />
      </div>

      <RouterLink
        to="/projects/new"
        class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-ink transition hover:opacity-90"
      >
        新建项目
      </RouterLink>
    </div>

    <p v-if="actionError" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
      {{ actionError }}
    </p>

    <p v-if="projects.error.value" class="rounded border border-status-failed/40 bg-surface-raised p-3 text-sm text-status-failed">
      {{ projects.error.value }}
      <AppButton variant="ghost" size="sm" class="ml-2" @click="projects.run">重试</AppButton>
    </p>

    <div v-else-if="projects.loading.value && !projects.data.value" class="p-6 text-center text-sm text-ink-muted">
      加载中…
    </div>

    <div
      v-else-if="projects.data.value && projects.data.value.items.length === 0"
      class="rounded border border-dashed border-line p-10 text-center text-sm text-ink-muted"
    >
      还没有项目。
      <RouterLink to="/projects/new" class="text-accent hover:underline">创建一个</RouterLink>
    </div>

    <div v-else-if="projects.data.value" class="overflow-x-auto rounded border border-line">
      <table class="w-full border-collapse text-sm">
        <thead>
          <tr class="border-b border-line text-left text-xs text-ink-faint">
            <th class="px-3 py-2 font-medium">名称</th>
            <th class="px-3 py-2 font-medium">语言对</th>
            <th class="px-3 py-2 font-medium">风格</th>
            <th class="px-3 py-2 font-medium">时长</th>
            <th class="px-3 py-2 font-medium">状态</th>
            <th class="px-3 py-2 font-medium"></th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="project in projects.data.value.items"
            :key="project.id"
            class="border-b border-line/60 transition hover:bg-surface-raised"
          >
            <td class="px-3 py-2">
              <RouterLink :to="`/projects/${project.id}`" class="hover:text-accent">
                {{ project.name }}
              </RouterLink>
            </td>
            <td class="px-3 py-2 text-ink-muted">
              {{ project.source_language }} → {{ project.target_language }}
            </td>
            <td class="px-3 py-2 text-ink-muted">{{ project.style }}</td>
            <td class="px-3 py-2 tabular-nums text-ink-muted">{{ formatDuration(project.duration) }}</td>
            <td class="px-3 py-2">
              <span class="text-ink-muted">{{ stageLabel(project) }}</span>
              <!--
                The review count is shown only when it is non-zero. A permanent
                "0 待审校" column trains people to stop reading it.
              -->
              <span v-if="project.needs_review_count > 0" class="ml-2 text-status-running">
                {{ project.needs_review_count }} 待审校
              </span>
            </td>
            <td class="px-3 py-2 text-right">
              <AppButton variant="ghost" size="sm" @click="remove(project)">
                删除
              </AppButton>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
